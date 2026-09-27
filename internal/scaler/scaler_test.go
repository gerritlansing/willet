package scaler

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Fakes

// fakeVM models a runner VM. Done closes when the runner exits by itself
// (exit) or when a Destroy succeeds. destroyErrs fail the first Destroy calls.
type fakeVM struct {
	name string
	done chan struct{}
	prov *fakeProv

	mu          sync.Mutex
	exitOnce    sync.Once
	exitCode    int
	destroyed   bool
	destroyErrs []error
	destroys    int
	destroyCtx  []error // ctx.Err() seen by each Destroy call
}

func (v *fakeVM) Done() <-chan struct{} { return v.done }

func (v *fakeVM) Result() (int, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.exitCode, nil
}

func (v *fakeVM) exit(code int) {
	v.exitOnce.Do(func() {
		v.mu.Lock()
		v.exitCode = code
		v.mu.Unlock()
		close(v.done)
	})
}

func (v *fakeVM) Destroy(ctx context.Context) error {
	v.mu.Lock()
	v.destroys++
	v.destroyCtx = append(v.destroyCtx, ctx.Err())
	if v.destroyed {
		v.mu.Unlock()
		return nil
	}
	if err := ctx.Err(); err != nil {
		v.mu.Unlock()
		return err
	}
	if len(v.destroyErrs) > 0 {
		err := v.destroyErrs[0]
		v.destroyErrs = v.destroyErrs[1:]
		v.mu.Unlock()
		return err
	}
	v.destroyed = true
	v.mu.Unlock()
	v.prov.live.Add(-1)
	v.exit(137)
	return nil
}

func (v *fakeVM) destroyCalls() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.destroys
}

func (v *fakeVM) isDestroyed() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.destroyed
}

type fakeProv struct {
	mu   sync.Mutex
	vms  map[string]*fakeVM
	fail bool
	// block makes Start wait for ctx to end, like a stalled image pull.
	block bool
	// gate, if set, holds Start until it is closed.
	gate chan struct{}
	// failWithVM makes the next Start return an already-exited VM along with
	// an error, like a runner that failed to launch in a VM that couldn't be
	// destroyed yet.
	failWithVM bool
	// destroyErrs is given to each new VM.
	destroyErrs []error

	live     atomic.Int64 // VMs started and not yet destroyed
	maxLive  atomic.Int64
	started  atomic.Int64
	canceled atomic.Int64
}

func (p *fakeProv) Start(ctx context.Context, name, _ string) (RunnerVM, error) {
	p.started.Add(1)
	if p.gate != nil {
		<-p.gate
	}
	if p.block {
		<-ctx.Done()
		p.canceled.Add(1)
		return nil, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return nil, errors.New("boom")
	}
	vm := &fakeVM{name: name, done: make(chan struct{}), prov: p, destroyErrs: append([]error(nil), p.destroyErrs...)}
	p.vms[name] = vm
	if p.failWithVM {
		p.failWithVM = false
		p.live.Add(1)
		vm.exit(-1)
		return vm, errors.New("runner failed to launch; VM teardown failed")
	}
	n := p.live.Add(1)
	for {
		m := p.maxLive.Load()
		if n <= m || p.maxLive.CompareAndSwap(m, n) {
			break
		}
	}
	return vm, nil
}

func (p *fakeProv) get(name string) *fakeVM {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.vms[name]
}

type fakeClient struct {
	mu      sync.Mutex
	nextID  int
	ids     map[string]int64
	removed map[int64]bool
	// busy runner IDs cannot be removed, like the real API.
	busy map[int64]bool
	jits int
	// removeBlock makes RemoveRunner wait for ctx to end, like a stalled API.
	removeBlock bool
	// removeHang makes RemoveRunner ignore ctx entirely until released, like
	// a call queued on the scaleset client's non-context-aware mutex.
	removeHang chan struct{}
	// removeCalls counts RemoveRunner calls; removeDeadline records the
	// deadline of the last one.
	removeCalls    int
	removeDeadline time.Time
}

func newFakeClient() *fakeClient {
	return &fakeClient{ids: map[string]int64{}, removed: map[int64]bool{}, busy: map[int64]bool{}}
}

func (c *fakeClient) GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jits++ // counts admissions, even ones the context then rejects
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.nextID++
	c.ids[s.Name] = int64(c.nextID)
	return &scaleset.RunnerScaleSetJitRunnerConfig{
		Runner:           &scaleset.RunnerReference{ID: c.nextID, Name: s.Name},
		EncodedJITConfig: "jit",
	}, nil
}

func (c *fakeClient) RemoveRunner(ctx context.Context, id int64) error {
	c.mu.Lock()
	hang, block := c.removeHang, c.removeBlock
	c.removeCalls++
	c.removeDeadline, _ = ctx.Deadline()
	c.mu.Unlock()
	if hang != nil {
		<-hang
	}
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.busy[id] {
		return errors.New("runner is busy")
	}
	c.removed[id] = true
	return nil
}

func (c *fakeClient) setBusy(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busy[c.ids[name]] = true
}

func (c *fakeClient) wasRemoved(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.removed[c.ids[name]]
}

func (c *fakeClient) registered() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name, id := range c.ids {
		if !c.removed[id] {
			out = append(out, name)
		}
	}
	return out
}

func (c *fakeClient) jitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jits
}

// ---------------------------------------------------------------------------
// Helpers

func newTest(t *testing.T, minR, maxR int, mutate ...func(*Config)) (*Scaler, *fakeProv, *fakeClient) {
	t.Helper()
	return newTestLogging(t, testLogger(), minR, maxR, mutate...)
}

func newTestLogging(t *testing.T, logger *slog.Logger, minR, maxR int, mutate ...func(*Config)) (*Scaler, *fakeProv, *fakeClient) {
	t.Helper()
	prov := &fakeProv{vms: map[string]*fakeVM{}}
	client := newFakeClient()
	cfg := Config{ScaleSetID: 1, NamePrefix: "t", MinRunners: minR, MaxRunners: maxR, CompletionGrace: 50 * time.Millisecond, APITimeout: time.Second}
	for _, m := range mutate {
		m(&cfg)
	}
	s := New(context.Background(), cfg, client, prov, logger)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return s, prov, client
}

func (s *Scaler) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.runners {
		out = append(out, n)
	}
	return out
}

func (s *Scaler) stateOf(name string) (state, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runners[name]
	if !ok {
		return 0, false
	}
	return r.state, true
}

// settled waits until no runner is still starting.
func (s *Scaler) settled(t *testing.T) {
	t.Helper()
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, r := range s.runners {
			if r.state == stateStarting {
				return false
			}
		}
		return true
	})
}

func reconcile(t *testing.T, s *Scaler, assigned int) int {
	t.Helper()
	got, err := s.HandleDesiredRunnerCount(context.Background(), assigned)
	if err != nil {
		t.Fatal(err)
	}
	s.settled(t)
	return got
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Scaling behaviour

func TestScaleUpRespectsMinAndMax(t *testing.T) {
	s, _, _ := newTest(t, 1, 3)

	if got := reconcile(t, s, 0); got != 1 || len(s.names()) != 1 {
		t.Fatalf("idle: target %d runners %d, want 1/1", got, len(s.names()))
	}
	if got := reconcile(t, s, 1); got != 2 || len(s.names()) != 2 {
		t.Fatalf("one job: target %d runners %d, want 2/2", got, len(s.names()))
	}
	if got := reconcile(t, s, 10); got != 3 || len(s.names()) != 3 {
		t.Fatalf("capped: target %d runners %d, want 3/3", got, len(s.names()))
	}
}

func TestJobLifecycleReclaimsVM(t *testing.T) {
	s, prov, client := newTest(t, 0, 2)

	reconcile(t, s, 1)
	name := s.names()[0]
	vm := prov.get(name)

	_ = s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name})
	_ = s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name, Result: "succeeded"})
	vm.exit(0) // the ephemeral runner exits after its job

	eventually(t, func() bool { return len(s.names()) == 0 })
	if !vm.isDestroyed() {
		t.Fatal("VM was not destroyed")
	}
	if client.wasRemoved(name) {
		t.Fatal("runner that ran a job deregisters itself; it should not be removed")
	}
}

func TestCompletionGraceDestroysStuckVM(t *testing.T) {
	s, prov, _ := newTest(t, 0, 1)

	reconcile(t, s, 1)
	name := s.names()[0]
	_ = s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name})
	_ = s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name})

	eventually(t, func() bool { return len(s.names()) == 0 })
	if !prov.get(name).isDestroyed() {
		t.Fatal("stuck VM was not destroyed after the grace period")
	}
}

func TestUnexpectedExitDeregistersRunner(t *testing.T) {
	s, prov, client := newTest(t, 1, 1)

	reconcile(t, s, 0)
	name := s.names()[0]
	prov.get(name).exit(1)

	eventually(t, func() bool { return len(s.names()) == 0 })
	if !client.wasRemoved(name) {
		t.Fatal("crashed idle runner was not deregistered")
	}

	// The next reconcile replaces it.
	reconcile(t, s, 0)
	if len(s.names()) != 1 {
		t.Fatalf("runners %d, want 1", len(s.names()))
	}
}

// A runner that exits 0 before any job message arrives is logged at info: it
// happens routinely when a short job finishes before GitHub reports it.
func TestExitLogLevel(t *testing.T) {
	for _, c := range []struct {
		code int
		want slog.Level
	}{{0, slog.LevelInfo}, {1, slog.LevelWarn}} {
		var mu sync.Mutex
		var levels []slog.Level
		h := &recordHandler{record: func(r slog.Record) {
			if strings.HasPrefix(r.Message, "Runner exited") {
				mu.Lock()
				levels = append(levels, r.Level)
				mu.Unlock()
			}
		}}
		s, prov, _ := newTestLogging(t, slog.New(h), 0, 1)

		reconcile(t, s, 1)
		prov.get(s.names()[0]).exit(c.code)
		eventually(t, func() bool { return len(s.names()) == 0 })

		mu.Lock()
		if len(levels) != 1 || levels[0] != c.want {
			t.Errorf("exit %d logged at %v, want [%v]", c.code, levels, c.want)
		}
		mu.Unlock()
	}
}

type recordHandler struct{ record func(slog.Record) }

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.record(r)
	return nil
}
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler      { return h }

func TestScaleDownSkipsBusyRunners(t *testing.T) {
	s, prov, client := newTest(t, 0, 3)

	reconcile(t, s, 3)
	names := s.names()
	busy, assigned, free := names[0], names[1], names[2]
	_ = s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: busy})
	client.setBusy(busy)
	// An idle runner the API reports as assigned must also survive.
	client.setBusy(assigned)

	reconcile(t, s, 0)

	eventually(t, func() bool { return len(s.names()) == 2 })
	for _, n := range []string{busy, assigned} {
		if prov.get(n).isDestroyed() {
			t.Fatalf("%s was destroyed", n)
		}
	}
	if st, _ := s.stateOf(assigned); st != stateIdle {
		t.Fatalf("assigned runner should return to idle after a refused removal, got state %d", st)
	}
	if !client.wasRemoved(free) {
		t.Fatal("free idle runner was not removed")
	}
}

func TestStartFailureDeregistersRunner(t *testing.T) {
	s, prov, client := newTest(t, 0, 1)
	prov.fail = true

	reconcile(t, s, 1)
	if len(s.names()) != 0 {
		t.Fatal("failed runner is still tracked")
	}
	if reg := client.registered(); len(reg) != 0 {
		t.Fatalf("runners left registered: %v", reg)
	}
}

// ---------------------------------------------------------------------------
// F01: capacity includes VMs that are draining or being torn down

func TestCapacityCountsDrainingVMs(t *testing.T) {
	s, prov, _ := newTest(t, 0, 1, func(c *Config) { c.CompletionGrace = time.Hour })

	reconcile(t, s, 1)
	first := s.names()[0]
	_ = s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: first})
	_ = s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: first})

	// The runner process keeps running after completing its job. New demand
	// must not push the number of VMs over the cap.
	for range 5 {
		reconcile(t, s, 1)
	}
	if n := prov.maxLive.Load(); n > 1 {
		t.Fatalf("%d VMs alive at once with max-runners 1", n)
	}
	if n := prov.started.Load(); n != 1 {
		t.Fatalf("%d VMs started while the slot was occupied", n)
	}

	// Once the old VM is gone, the slot is released and new work starts.
	prov.get(first).exit(0)
	eventually(t, func() bool { _, ok := s.stateOf(first); return !ok })
	reconcile(t, s, 1)
	if n := prov.started.Load(); n != 2 {
		t.Fatalf("started %d VMs, want the replacement to start", n)
	}
	if n := prov.maxLive.Load(); n > 1 {
		t.Fatalf("%d VMs alive at once", n)
	}
}

// Found by the real-VM integration test: a job can start and complete before
// Start returns the VM. The grace period must still apply once the VM exists.
func TestJobCompletedWhileVMStillBooting(t *testing.T) {
	s, prov, _ := newTest(t, 0, 1)
	prov.gate = make(chan struct{})

	if _, err := s.HandleDesiredRunnerCount(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return prov.started.Load() == 1 })
	name := s.names()[0]
	_ = s.HandleJobStarted(context.Background(), &scaleset.JobStarted{RunnerName: name})
	_ = s.HandleJobCompleted(context.Background(), &scaleset.JobCompleted{RunnerName: name})
	close(prov.gate) // Start returns now; the runner process never exits

	eventually(t, func() bool { _, ok := s.stateOf(name); return !ok })
	if !prov.get(name).isDestroyed() {
		t.Fatal("VM of a job completed during boot was never reclaimed")
	}
}

// ---------------------------------------------------------------------------
// F03: failed destroys are retried and keep their slot

func TestFailedDestroyIsRetriedAndKeepsSlot(t *testing.T) {
	s, prov, _ := newTest(t, 0, 1)
	prov.destroyErrs = []error{errors.New("runtime hiccup"), errors.New("runtime hiccup")}

	reconcile(t, s, 1)
	name := s.names()[0]
	vm := prov.get(name)
	prov.mu.Lock()
	prov.destroyErrs = nil // only the first VM is faulty
	prov.mu.Unlock()
	vm.exit(0)

	// The first destroy fails: the runner stays tracked and holds the slot.
	eventually(t, func() bool { st, ok := s.stateOf(name); return ok && st == stateReclaiming && vm.destroyCalls() >= 1 })
	reconcile(t, s, 1)
	if n := prov.started.Load(); n != 1 {
		t.Fatalf("started a replacement while a failed VM still holds the slot")
	}

	// Each reconcile retries; the second retry succeeds and frees the slot.
	eventually(t, func() bool {
		reconcile(t, s, 0)
		return vm.isDestroyed()
	})
	eventually(t, func() bool { _, ok := s.stateOf(name); return !ok })
	reconcile(t, s, 1)
	if n := prov.started.Load(); n != 2 {
		t.Fatalf("replacement did not start after the slot was released")
	}
}

func TestDestroyRetriesStopAfterLimitButSlotStaysReserved(t *testing.T) {
	s, prov, _ := newTest(t, 0, 1, func(c *Config) { c.MaxDestroyAttempts = 2 })
	prov.destroyErrs = []error{errors.New("x"), errors.New("x"), errors.New("x"), errors.New("x")}

	reconcile(t, s, 1)
	name := s.names()[0]
	vm := prov.get(name)
	vm.exit(0)

	eventually(t, func() bool {
		reconcile(t, s, 1)
		return vm.destroyCalls() >= 2
	})
	for range 5 {
		reconcile(t, s, 1)
	}
	if attempts := vm.destroyCalls(); attempts != 2 {
		t.Fatalf("%d destroy attempts, want retries to stop at 2", attempts)
	}
	if n := prov.started.Load(); n != 1 {
		t.Fatal("capacity was released for a VM that was never destroyed")
	}
}

// R01: a VM returned with a start error still exists, so it keeps its slot
// until destroyed, and its registration is removed.
func TestFailedStartVMKeepsSlotUntilDestroyed(t *testing.T) {
	s, prov, client := newTest(t, 0, 1)
	prov.failWithVM = true
	prov.destroyErrs = []error{errors.New("x"), errors.New("x")}

	reconcile(t, s, 1)
	names := s.names()
	if len(names) != 1 {
		t.Fatalf("the VM from the failed start is not tracked: %v", names)
	}
	name := names[0]
	vm := prov.get(name)
	prov.mu.Lock()
	prov.destroyErrs = nil // later VMs are healthy
	prov.mu.Unlock()

	eventually(t, func() bool { st, _ := s.stateOf(name); return st == stateReclaiming && vm.destroyCalls() >= 1 })
	reconcile(t, s, 1)
	if n := prov.maxLive.Load(); n > 1 {
		t.Fatalf("%d VMs alive with max-runners 1: the failed VM's slot was released", n)
	}
	if !client.wasRemoved(name) {
		t.Fatal("runner registered for the failed start was not removed")
	}

	eventually(t, func() bool {
		reconcile(t, s, 1)
		return vm.isDestroyed()
	})
	eventually(t, func() bool { _, ok := s.stateOf(name); return !ok })
	reconcile(t, s, 1)
	if n := prov.started.Load(); n < 2 {
		t.Fatal("no replacement started after the failed VM was destroyed")
	}
	if n := prov.maxLive.Load(); n > 1 {
		t.Fatalf("%d VMs alive at once", n)
	}
}

// ---------------------------------------------------------------------------
// F04: shutdown reclaims locally regardless of GitHub

func TestShutdownDestroysVMsEvenWhenGitHubStalls(t *testing.T) {
	s, prov, client := newTest(t, 2, 2)
	reconcile(t, s, 0)
	names := s.names()

	client.mu.Lock()
	client.removeBlock = true
	client.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	begin := time.Now()
	s.Shutdown(ctx)
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("shutdown took %s, beyond its deadline", d)
	}
	for _, n := range names {
		vm := prov.get(n)
		if !vm.isDestroyed() {
			t.Fatalf("%s was not destroyed", n)
		}
		if err := vm.destroyCtx[0]; err != nil {
			t.Fatalf("%s: destroy got an already-expired context: %v", n, err)
		}
	}
}

func TestShutdownReturnsWhileAPICallIgnoresContext(t *testing.T) {
	s, prov, client := newTest(t, 1, 1)
	reconcile(t, s, 0)
	name := s.names()[0]

	// A background deregistration gets stuck on the client, ignoring its
	// context, like a call queued on scaleset's non-context-aware mutex.
	hang := make(chan struct{})
	defer close(hang)
	client.mu.Lock()
	client.removeHang = hang
	client.mu.Unlock()
	prov.get(name).exit(1) // unexpected exit triggers a deregistration

	time.Sleep(20 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	begin := time.Now()
	s.Shutdown(ctx)
	if d := time.Since(begin); d > 700*time.Millisecond {
		t.Fatalf("shutdown took %s despite a 300ms budget", d)
	}
}

func TestShutdownWithFailingDestroyReturnsPromptly(t *testing.T) {
	s, prov, _ := newTest(t, 1, 1)
	prov.destroyErrs = []error{errors.New("x"), errors.New("x")}
	reconcile(t, s, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	begin := time.Now()
	s.Shutdown(ctx)
	if d := time.Since(begin); d > 200*time.Millisecond {
		t.Fatalf("shutdown waited %s for a VM whose destroy failed", d)
	}
}

func TestShutdownDestroysAll(t *testing.T) {
	s, prov, _ := newTest(t, 2, 2)
	reconcile(t, s, 0)
	names := s.names()

	s.Shutdown(context.Background())
	for _, n := range names {
		if !prov.get(n).isDestroyed() {
			t.Fatalf("%s survived shutdown", n)
		}
	}
	if len(s.names()) != 0 {
		t.Fatal("runners still tracked after shutdown")
	}
}

// ---------------------------------------------------------------------------
// F05 and the closed-scaler leak: starts are cancelable and admission-checked

func TestReconcileDoesNotBlockOnStarts(t *testing.T) {
	s, prov, _ := newTest(t, 0, 2)
	prov.block = true

	begin := time.Now()
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d > 100*time.Millisecond {
		t.Fatalf("reconcile blocked for %s on a stalled VM boot", d)
	}
}

func TestShutdownCancelsStalledStarts(t *testing.T) {
	s, prov, client := newTest(t, 0, 3, func(c *Config) { c.StartConcurrency = 1 })
	prov.block = true

	// One start stalls in the provisioner; two more wait for a start slot.
	if _, err := s.HandleDesiredRunnerCount(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return prov.started.Load() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	begin := time.Now()
	s.Shutdown(ctx)
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Fatalf("shutdown took %s; stalled starts were not canceled", d)
	}
	if n := prov.canceled.Load(); n != 1 {
		t.Fatalf("%d starts observed cancellation in the provisioner, want exactly the 1 that was stalled", n)
	}
	if n := prov.started.Load(); n != 1 {
		t.Fatalf("%d starts reached the provisioner, want 1: queued starts ran after shutdown", n)
	}
	if n := client.jitCount(); n != 1 {
		t.Fatalf("%d runners registered, want 1: queued starts reached GitHub after shutdown", n)
	}
	if reg := client.registered(); len(reg) != 0 {
		t.Fatalf("canceled start left runners registered: %v", reg)
	}
	if len(s.names()) != 0 {
		t.Fatal("canceled starts still tracked")
	}
}

// Found in review: when cancellation and a freed start slot are both ready,
// select may pick the slot. The start must still not reach GitHub.
func TestCanceledStartWinningSlotMakesNoAPICalls(t *testing.T) {
	for i := range 200 {
		s, prov, client := newTest(t, 0, 1, func(c *Config) { c.StartConcurrency = 1 })
		s.sem <- struct{}{} // occupy the only slot
		r := &runner{name: "t-x", state: stateStarting, gone: make(chan struct{})}
		s.mu.Lock()
		s.runners[r.name] = r
		s.mu.Unlock()

		done := make(chan struct{})
		go func() { s.start(r); close(done) }()
		s.cancel() // lifetime ends, as on SIGTERM
		<-s.sem    // the slot frees at the same time
		<-done

		if n := client.jitCount(); n != 0 {
			t.Fatalf("iteration %d: canceled start registered %d runners", i, n)
		}
		if n := prov.started.Load(); n != 0 {
			t.Fatalf("iteration %d: canceled start booted %d VMs", i, n)
		}
		if _, ok := s.stateOf(r.name); ok {
			t.Fatalf("iteration %d: canceled start still tracked", i)
		}
	}
}

func TestClosedScalerMakesNoCalls(t *testing.T) {
	s, prov, client := newTest(t, 0, 5)
	s.Shutdown(context.Background())

	reconcile(t, s, 5)
	if client.jitCount() != 0 || prov.started.Load() != 0 {
		t.Fatalf("closed scaler registered %d runners and started %d VMs", client.jitCount(), prov.started.Load())
	}
}

// ---------------------------------------------------------------------------
// The real listener: message-driven starts must stop on cancellation even
// though the listener handles messages under context.WithoutCancel.

type fakeSession struct {
	sent atomic.Bool
}

func (f *fakeSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	if f.sent.CompareAndSwap(false, true) {
		return &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{TotalAssignedJobs: 2}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f *fakeSession) DeleteMessage(context.Context, int) error              { return nil }
func (f *fakeSession) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }
func (f *fakeSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

func TestListenerMessageDrivenStartsAreCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prov := &fakeProv{vms: map[string]*fakeVM{}, block: true}
	client := newFakeClient()
	s := New(ctx, Config{ScaleSetID: 1, NamePrefix: "t", MaxRunners: 2}, client, prov, slog.New(slog.DiscardHandler))

	l, err := listener.New(&fakeSession{}, listener.Config{ScaleSetID: 1, MaxRunners: 2})
	if err != nil {
		t.Fatal(err)
	}
	runErr := make(chan error, 1)
	go func() { runErr <- l.Run(ctx, s) }()

	eventually(t, func() bool { return prov.started.Load() == 2 })

	cancel() // SIGTERM
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("listener: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("listener did not return after cancellation")
	}

	shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	begin := time.Now()
	s.Shutdown(shutdownCtx)
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Fatalf("shutdown took %s", d)
	}
	if prov.canceled.Load() != 2 {
		t.Fatalf("%d of 2 stalled starts observed cancellation", prov.canceled.Load())
	}
	if reg := client.registered(); len(reg) != 0 {
		t.Fatalf("runners left registered: %v", reg)
	}
}

func testLogger() *slog.Logger {
	if os.Getenv("SCALER_TEST_LOG") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.DiscardHandler)
}

// M03: deregister uses an ordinary child context: a canceled parent stops it
// before calling GitHub, a short parent deadline wins over APITimeout, and a
// background parent still gets APITimeout.
func TestDeregisterContext(t *testing.T) {
	s, _, client := newTest(t, 0, 1, func(c *Config) { c.APITimeout = time.Hour })
	r := &runner{name: "t-x", id: 42, gone: make(chan struct{})}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	s.deregister(canceled, r)
	if client.removeCalls != 0 {
		t.Fatal("deregister called GitHub with an already-canceled parent")
	}

	short, cancelShort := context.WithTimeout(context.Background(), time.Minute)
	defer cancelShort()
	s.deregister(short, r)
	if client.removeCalls != 1 || time.Until(client.removeDeadline) > time.Minute {
		t.Fatalf("calls=%d deadline in %s; want the parent's 1m deadline", client.removeCalls, time.Until(client.removeDeadline))
	}
	if !client.removed[42] {
		t.Fatal("runner not removed")
	}

	r2 := &runner{name: "t-y", id: 43, gone: make(chan struct{})}
	s.deregister(context.Background(), r2)
	if d := time.Until(client.removeDeadline); d < 59*time.Minute {
		t.Fatalf("background deregister got a %s deadline; want APITimeout (1h)", d)
	}
}

// M05: only these states count as able to take a job; any other, including
// one added later, must not.
func TestSchedulableStates(t *testing.T) {
	for st, want := range map[state]bool{
		stateStarting: true, stateIdle: true, stateBusy: true,
		stateDraining: false, stateReclaiming: false, state(99): false,
	} {
		if got := st.schedulable(); got != want {
			t.Errorf("state %d: schedulable=%v, want %v", st, got, want)
		}
	}
}
