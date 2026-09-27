// Package scaler reconciles the number of runner VMs with the demand reported
// by the GitHub Actions scale set API.
package scaler

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

// RunnerVM is a running runner VM.
type RunnerVM interface {
	// Done is closed when the runner process has exited.
	Done() <-chan struct{}
	// Result returns the runner exit code and stream error once Done is closed.
	Result() (int, error)
	// Destroy tears the VM down. It returns nil only once the VM is gone, and
	// may be called again after an error.
	Destroy(ctx context.Context) error
}

// Provisioner boots runner VMs.
type Provisioner interface {
	// Start boots a VM running the runner. If it returns a non-nil VM along
	// with an error, the VM exists although the runner did not launch, and
	// it must still be destroyed.
	Start(ctx context.Context, name, jitConfig string) (RunnerVM, error)
}

// Client is the subset of *scaleset.Client the scaler needs.
type Client interface {
	GenerateJitRunnerConfig(ctx context.Context, setting *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
}

// Config configures a Scaler.
type Config struct {
	ScaleSetID int
	// NamePrefix prefixes runner and VM names.
	NamePrefix string
	MinRunners int
	// MaxRunners caps VMs that exist at once, in any state: booting, running,
	// or being torn down.
	MaxRunners int
	// StartConcurrency bounds how many VMs boot in parallel. Default 4.
	StartConcurrency int
	// StartTimeout bounds registering and booting one runner, including any
	// image pull. Default 10m.
	StartTimeout time.Duration
	// CompletionGrace is how long a runner may linger after its job completed
	// before its VM is destroyed forcibly. Default 2m.
	CompletionGrace time.Duration
	// APITimeout bounds each GitHub API call. Default 30s.
	APITimeout time.Duration
	// MaxDestroyAttempts is how often a failed VM destroy is retried before the
	// scaler gives up. The VM keeps its capacity slot either way. Default 10.
	MaxDestroyAttempts int
}

func (c *Config) defaults() {
	if c.StartConcurrency <= 0 {
		c.StartConcurrency = 4
	}
	if c.StartTimeout <= 0 {
		c.StartTimeout = 10 * time.Minute
	}
	if c.CompletionGrace <= 0 {
		c.CompletionGrace = 2 * time.Minute
	}
	if c.APITimeout <= 0 {
		c.APITimeout = 30 * time.Second
	}
	if c.MaxDestroyAttempts <= 0 {
		c.MaxDestroyAttempts = 10
	}
}

type state int

const (
	// stateStarting: slot reserved; registering the runner and booting its VM.
	stateStarting state = iota
	// stateIdle: runner process up, no job yet.
	stateIdle
	// stateBusy: running a job.
	stateBusy
	// stateDraining: job completed or scale-down requested; the VM is on its way out.
	stateDraining
	// stateReclaiming: VM being destroyed, or a destroy failed and awaits retry.
	stateReclaiming
)

// schedulable reports whether a runner in this state can take, or is taking,
// a job. States are listed explicitly so that a new state is never
// schedulable by accident.
func (st state) schedulable() bool {
	switch st {
	case stateStarting, stateIdle, stateBusy:
		return true
	default:
		return false
	}
}

// runner is one tracked runner and its VM. It holds a capacity slot from
// creation until forget.
//
// Valid combinations of state and flags:
//   - scalingDown only in stateDraining: a scale-down removal request is in
//     flight. It ends via endScaleDown, or when a job wins the race.
//   - reclaimInFlight only in stateReclaiming: a teardown is running now.
//     stateReclaiming without it, and with destroyAttempts > 0, means a
//     teardown failed and awaits retry (see awaitingRetry).
//   - ranJob is history (the runner started a job at some point); it decides
//     whether the runner must still be deregistered, not what it does next.
type runner struct {
	name string
	// gone is closed when the runner is removed from tracking.
	gone     chan struct{}
	goneOnce sync.Once

	// Fields below are guarded by Scaler.mu.
	id              int64
	vm              RunnerVM
	state           state
	ranJob          bool
	scalingDown     bool
	deregistered    bool
	reclaimInFlight bool
	destroyAttempts int
}

// beginScaleDown marks an idle runner as being removed.
func (r *runner) beginScaleDown() {
	r.state = stateDraining
	r.scalingDown = true
}

// endScaleDown records the result of a scale-down removal request. A refused
// removal returns the runner to idle, unless a job has taken it meanwhile.
func (r *runner) endScaleDown(removed bool) {
	if removed {
		r.deregistered = true
	} else if r.scalingDown && r.state == stateDraining {
		r.state = stateIdle
	}
	r.scalingDown = false
}

// awaitingRetry reports whether a failed teardown should be retried now.
func (r *runner) awaitingRetry(maxAttempts int) bool {
	return r.state == stateReclaiming && !r.reclaimInFlight &&
		r.destroyAttempts > 0 && r.destroyAttempts < maxAttempts
}

// Scaler implements listener.Scaler by booting one microVM per ephemeral runner.
//
// Every runner holds a capacity slot from the moment its start is scheduled
// until its VM is confirmed destroyed, so the number of VMs never exceeds
// MaxRunners. Starts, scale-downs and teardown run in the background under the
// scaler's own lifetime context, so message handling never blocks on them and
// Shutdown can always cancel them.
type Scaler struct {
	cfg    Config
	client Client
	prov   Provisioner
	logger *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}

	mu      sync.Mutex
	runners map[string]*runner
	closing bool
	// wg tracks background goroutines. Add is only called under mu while
	// !closing, so it never races with Shutdown's Wait.
	wg sync.WaitGroup
}

var _ listener.Scaler = (*Scaler)(nil)

// New returns a Scaler. Background work stops when ctx is canceled or
// Shutdown is called.
func New(ctx context.Context, cfg Config, client Client, prov Provisioner, logger *slog.Logger) *Scaler {
	cfg.defaults()
	ctx, cancel := context.WithCancel(ctx)
	return &Scaler{
		cfg:     cfg,
		client:  client,
		prov:    prov,
		logger:  logger,
		ctx:     ctx,
		cancel:  cancel,
		sem:     make(chan struct{}, cfg.StartConcurrency),
		runners: make(map[string]*runner),
	}
}

// HandleDesiredRunnerCount converges on min(maxRunners, minRunners+assignedJobs)
// schedulable runners, without exceeding MaxRunners VMs in total. It only
// schedules work and returns immediately. It is called after every message
// and every empty long poll, so it also retries failed VM destroys.
func (s *Scaler) HandleDesiredRunnerCount(_ context.Context, assignedJobs int) (int, error) {
	target := min(s.cfg.MaxRunners, s.cfg.MinRunners+assignedJobs)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return target, nil
	}

	schedulable, allocated := 0, len(s.runners)
	for _, r := range s.runners {
		if r.state.schedulable() {
			schedulable++
		}
		if r.awaitingRetry(s.cfg.MaxDestroyAttempts) {
			s.logger.Info("Retrying VM destroy", slog.String("runner", r.name), slog.Int("attempt", r.destroyAttempts+1))
			s.beginReclaimLocked(r)
			s.goLocked(func() { s.finishReclaim(r) })
		}
	}

	switch {
	case target > schedulable:
		n := min(target-schedulable, s.cfg.MaxRunners-allocated)
		if n <= 0 {
			s.logger.Info("At capacity, waiting for VMs to be torn down",
				slog.Int("schedulable", schedulable), slog.Int("allocated", allocated), slog.Int("target", target))
			break
		}
		s.logger.Info("Scaling up", slog.Int("schedulable", schedulable), slog.Int("target", target), slog.Int("starting", n))
		for range n {
			r := &runner{name: s.newName(), state: stateStarting, gone: make(chan struct{})}
			s.runners[r.name] = r
			s.goLocked(func() { s.run(r) })
		}
	case target < schedulable:
		excess := schedulable - target
		for _, r := range s.runners {
			if excess == 0 {
				break
			}
			if r.state == stateIdle && r.id != 0 {
				r.beginScaleDown()
				s.goLocked(func() { s.scaleDown(r) })
				excess--
			}
		}
	}
	return target, nil
}

// HandleJobStarted marks the runner busy so it is never scaled down.
func (s *Scaler) HandleJobStarted(_ context.Context, job *scaleset.JobStarted) error {
	s.logger.Info("Job started", slog.String("runner", job.RunnerName), slog.String("jobId", job.JobID))
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runners[job.RunnerName]; ok {
		r.ranJob = true
		// A job can win the race against a pending scale-down.
		if r.state.schedulable() || r.scalingDown {
			r.state = stateBusy
			r.scalingDown = false
		}
	}
	return nil
}

// HandleJobCompleted starts draining the runner. The ephemeral runner exits by
// itself right after its job; if it doesn't within CompletionGrace, its VM is
// destroyed.
func (s *Scaler) HandleJobCompleted(_ context.Context, job *scaleset.JobCompleted) error {
	s.logger.Info("Job completed", slog.String("runner", job.RunnerName), slog.String("jobId", job.JobID), slog.String("result", job.Result))
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runners[job.RunnerName]
	if !ok || s.closing {
		return nil
	}
	r.ranJob = true
	if r.state.schedulable() {
		r.state = stateDraining
	}
	// A runner can finish a job before Start has returned its VM; start()
	// arms the grace period in that case.
	if r.vm != nil {
		s.armGraceLocked(r)
	}
	return nil
}

// armGraceLocked destroys the runner's VM if it hasn't exited within
// CompletionGrace. Callers must hold s.mu, with r.vm set and !s.closing.
func (s *Scaler) armGraceLocked(r *runner) {
	vm := r.vm
	s.goLocked(func() {
		t := time.NewTimer(s.cfg.CompletionGrace)
		defer t.Stop()
		select {
		case <-vm.Done():
		case <-r.gone:
		case <-s.ctx.Done():
		case <-t.C:
			s.logger.Warn("Runner still alive after job completed, destroying VM", slog.String("runner", r.name))
			s.reclaim(r)
		}
	})
}

// Shutdown stops all runners. VMs are destroyed locally first, within about
// two thirds of ctx's remaining time and without waiting on GitHub; runners
// are then deregistered with what remains. Shutdown returns by ctx's
// deadline even if background work is still stuck.
func (s *Scaler) Shutdown(ctx context.Context) {
	s.mu.Lock()
	s.closing = true
	var live, unused []*runner
	for _, r := range s.runners {
		if r.vm != nil {
			live = append(live, r)
			// Runners that ran a job are removed by GitHub itself.
			if !r.ranJob {
				unused = append(unused, r)
			}
		}
	}
	s.mu.Unlock()
	s.cancel() // aborts in-flight starts

	s.logger.Info("Shutting down runners", slog.Int("count", len(live)))

	destroyCtx, cancelDestroy := budget(ctx, 2, 3)
	defer cancelDestroy()
	var destroys sync.WaitGroup
	for _, r := range live {
		destroys.Go(func() {
			if err := r.vm.Destroy(destroyCtx); err != nil {
				s.logger.Error("Failed to destroy VM during shutdown", slog.String("runner", r.name), slog.String("error", err.Error()))
				return
			}
			s.forget(r)
		})
	}
	if !waitCtx(destroyCtx, &destroys) {
		s.logger.Error("Timed out destroying runner VMs")
	}

	var deregs sync.WaitGroup
	for _, r := range unused {
		deregs.Go(func() { s.deregister(ctx, r) })
	}
	if !waitCtx(ctx, &deregs) {
		s.logger.Warn("Timed out deregistering runners; GitHub removes offline ephemeral runners eventually")
	}

	if !waitCtx(ctx, &s.wg) {
		s.logger.Warn("Timed out waiting for background work to finish")
	}
}

// goLocked runs f in a tracked goroutine. Callers must hold s.mu and must
// have checked !s.closing.
func (s *Scaler) goLocked(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

func (s *Scaler) newName() string {
	return fmt.Sprintf("%s-%s", s.cfg.NamePrefix, strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
}

// run drives one runner from start to exit.
func (s *Scaler) run(r *runner) {
	vm, ok := s.start(r)
	if !ok {
		return
	}
	select {
	case <-vm.Done():
	case <-r.gone:
		return
	case <-s.ctx.Done():
		// Shutdown destroys every VM itself; don't hold it up waiting for an
		// exit that a failed destroy may never produce.
		return
	}

	code, err := vm.Result()
	attrs := []any{slog.String("runner", r.name), slog.Int("exitCode", code)}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	s.mu.Lock()
	expected := s.closing || r.state == stateDraining || r.state == stateReclaiming
	s.mu.Unlock()
	switch {
	case expected:
		s.logger.Info("Runner exited", attrs...)
	case code == 0 && err == nil:
		// Usually a short job that finished before GitHub's job messages
		// arrived. run.sh also exits 0 when the runner rejects its config, so
		// this doesn't prove a job ran, but it isn't a crash either.
		s.logger.Info("Runner exited on its own", attrs...)
	default:
		s.logger.Warn("Runner exited unexpectedly", attrs...)
	}
	s.reclaim(r)
}

// start registers the runner and boots its VM. On failure it undoes the
// registration and releases the slot, except when the provisioner hands back
// a VM it could not destroy: then the runner keeps its slot and goes through
// reclaim, which retries the teardown.
func (s *Scaler) start(r *runner) (RunnerVM, bool) {
	ctx, cancel := context.WithTimeout(s.ctx, s.cfg.StartTimeout)
	defer cancel()

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		s.forget(r)
		return nil, false
	}
	// A slot freeing up and cancellation can race; select picks either. Check
	// again so a canceled or closing scaler never registers a runner.
	s.mu.Lock()
	stopping := s.closing
	s.mu.Unlock()
	if stopping || ctx.Err() != nil {
		s.forget(r)
		return nil, false
	}

	apiCtx, cancelAPI := context.WithTimeout(ctx, s.cfg.APITimeout)
	jit, err := s.client.GenerateJitRunnerConfig(apiCtx, &scaleset.RunnerScaleSetJitRunnerSetting{Name: r.name}, s.cfg.ScaleSetID)
	cancelAPI()
	if err != nil {
		s.logStartFailure(r, fmt.Errorf("generate JIT config: %w", err))
		s.forget(r)
		return nil, false
	}
	if jit.Runner != nil {
		s.mu.Lock()
		r.id = int64(jit.Runner.ID)
		s.mu.Unlock()
	}

	began := time.Now()
	vm, err := s.prov.Start(ctx, r.name, jit.EncodedJITConfig)
	if err != nil && vm != nil {
		// The VM exists but its runner didn't launch, and it couldn't be torn
		// down yet. Keep it and its slot; reclaim retries the teardown.
		s.mu.Lock()
		r.vm = vm
		s.mu.Unlock()
		s.logStartFailure(r, fmt.Errorf("start VM: %w", err))
		s.reclaim(r)
		return nil, false
	}
	if err != nil {
		// Don't leave a registered runner behind that nothing will ever run.
		s.deregister(context.Background(), r)
		s.logStartFailure(r, fmt.Errorf("start VM: %w", err))
		s.forget(r)
		return nil, false
	}

	s.mu.Lock()
	r.vm = vm
	closing := s.closing
	switch {
	case r.state == stateStarting:
		r.state = stateIdle
	case r.state == stateDraining && !closing:
		// The job completed while the VM was still booting.
		s.armGraceLocked(r)
	}
	s.mu.Unlock()
	if closing {
		// Shutdown began while booting and may not have seen this VM.
		s.reclaim(r)
		return nil, false
	}
	s.logger.Info("Runner started", slog.String("runner", r.name), slog.Int64("runnerId", r.id), slog.Duration("boot", time.Since(began)))
	return vm, true
}

func (s *Scaler) logStartFailure(r *runner, err error) {
	if s.ctx.Err() != nil {
		s.logger.Info("Runner start canceled", slog.String("runner", r.name))
		return
	}
	// Not fatal: the next reconcile, within ~50s, starts a replacement.
	s.logger.Error("Failed to start runner", slog.String("runner", r.name), slog.String("error", err.Error()))
}

// scaleDown deregisters an idle runner and destroys its VM. GitHub refuses to
// remove a runner that has been assigned a job, so a job that wins the race
// keeps its runner.
func (s *Scaler) scaleDown(r *runner) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), s.cfg.APITimeout)
	err := s.client.RemoveRunner(ctx, r.id)
	cancel()

	s.mu.Lock()
	r.endScaleDown(err == nil)
	if err != nil {
		s.mu.Unlock()
		s.logger.Debug("Idle runner not removable, probably assigned a job", slog.String("runner", r.name), slog.String("error", err.Error()))
		return
	}
	s.mu.Unlock()
	s.logger.Info("Scaling down idle runner", slog.String("runner", r.name))
	s.reclaim(r)
}

// reclaim destroys the runner's VM and releases its slot. A runner that never
// ran a job may still be registered, so it is deregistered first. If the
// destroy fails the runner stays tracked, keeping its slot, and a later
// reconcile retries. Concurrent calls collapse into one.
func (s *Scaler) reclaim(r *runner) {
	s.mu.Lock()
	ok := s.beginReclaimLocked(r)
	s.mu.Unlock()
	if ok {
		s.finishReclaim(r)
	}
}

// beginReclaimLocked marks a reclaim in progress, reporting false if one
// already is. Callers must hold s.mu.
func (s *Scaler) beginReclaimLocked(r *runner) bool {
	if r.reclaimInFlight {
		return false
	}
	r.reclaimInFlight = true
	r.state = stateReclaiming
	return true
}

// finishReclaim does the work of a reclaim begun with beginReclaimLocked.
func (s *Scaler) finishReclaim(r *runner) {
	s.mu.Lock()
	ranJob, vm := r.ranJob, r.vm
	s.mu.Unlock()

	if !ranJob {
		s.deregister(context.Background(), r)
	}

	if vm != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), time.Minute)
		err := vm.Destroy(ctx)
		cancel()
		if err != nil {
			s.mu.Lock()
			r.destroyAttempts++
			attempts := r.destroyAttempts
			r.reclaimInFlight = false
			s.mu.Unlock()
			attrs := []any{slog.String("runner", r.name), slog.Int("attempt", attempts), slog.String("error", err.Error())}
			if attempts >= s.cfg.MaxDestroyAttempts {
				s.logger.Error("Giving up destroying VM; its capacity slot stays reserved until the daemon restarts. Check `msb ls`.", attrs...)
			} else {
				s.logger.Warn("Failed to destroy VM, will retry", attrs...)
			}
			return
		}
	}
	s.forget(r)
}

// deregister removes the runner from GitHub, at most once successfully.
func (s *Scaler) deregister(parent context.Context, r *runner) {
	s.mu.Lock()
	id, done := r.id, r.deregistered
	s.mu.Unlock()
	if id == 0 || done {
		return
	}
	// Bounded by APITimeout, or parent's earlier deadline. Callers that must
	// outlive the daemon's lifetime pass a context that isn't tied to it.
	ctx, cancel := context.WithTimeout(parent, s.cfg.APITimeout)
	defer cancel()
	if ctx.Err() != nil {
		return
	}
	if err := s.client.RemoveRunner(ctx, id); err != nil {
		s.logger.Debug("Could not deregister runner", slog.String("runner", r.name), slog.String("error", err.Error()))
		return
	}
	s.mu.Lock()
	r.deregistered = true
	s.mu.Unlock()
}

// forget stops tracking the runner, releasing its capacity slot.
func (s *Scaler) forget(r *runner) {
	s.mu.Lock()
	if s.runners[r.name] == r {
		delete(s.runners, r.name)
	}
	s.mu.Unlock()
	r.goneOnce.Do(func() { close(r.gone) })
}

// budget returns a context whose deadline is num/den of ctx's remaining time.
func budget(ctx context.Context, num, den int64) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return context.WithCancel(ctx)
	}
	remaining := time.Until(deadline)
	return context.WithTimeout(ctx, time.Duration(int64(remaining)*num/den))
}

// waitCtx waits for wg or ctx, reporting whether wg finished.
func waitCtx(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}
