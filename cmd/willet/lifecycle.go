package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// shutdownBudget bounds the whole shutdown. systemd's TimeoutStopSec in the
// README's unit is 90s, leaving headroom before SIGKILL.
const shutdownBudget = time.Minute

// bounded runs fn with a context that ends when parent ends or timeout
// passes, and returns as soon as that context ends even if fn does not. fn
// then keeps running in the background and is abandoned. This is needed for
// scaleset client calls, which wait on a mutex that ignores contexts.
func bounded[T any](parent context.Context, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := fn(ctx)
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// Shutdown runs in three phases under one deadline:
//
//  1. quiesce: stop background work that could create VMs (image refresh);
//  2. local: destroy VMs (runners, a leftover temporary VM), concurrently;
//  3. remote: GitHub cleanup (session, scale set).
//
// VM teardown always comes before session and scale-set cleanup, and the
// last remoteShare of the budget is reserved for those, so neither can starve
// the other. (Runner deregistration is part of stopping the runners, so it
// happens in the local phase, after each runner's VM is destroyed.) Only steps for resources that were actually created are registered,
// so an early startup failure cleans up what exists.

type shutdownPhase int

const (
	phaseQuiesce shutdownPhase = iota
	phaseLocal
	phaseRemote
)

// remoteShare of the budget is reserved for the remote phase.
const remoteShare = 0.25

type shutdownStep struct {
	name  string
	phase shutdownPhase
	// share caps the step at this fraction of the budget; 0 means it may
	// use the rest of its phase.
	share float64
	fn    func(context.Context) error
}

type shutdownSteps struct {
	logger *slog.Logger
	steps  []shutdownStep
}

func (s *shutdownSteps) add(st shutdownStep) { s.steps = append(s.steps, st) }

// The daemon's shutdown steps. run() registers these as it creates the
// corresponding resources; tests use the same constructors.

func stopRefreshStep(stop func(context.Context) error) shutdownStep {
	return shutdownStep{name: "stop image refresh", phase: phaseQuiesce, share: 1.0 / 6, fn: stop}
}

func stopRunnersStep(shutdown func(context.Context)) shutdownStep {
	return shutdownStep{name: "stop runners", phase: phaseLocal, fn: func(ctx context.Context) error {
		shutdown(ctx)
		return nil
	}}
}

func destroyTempVMStep(closeFn func(context.Context) error) shutdownStep {
	return shutdownStep{name: "destroy leftover temporary VM", phase: phaseLocal, fn: closeFn}
}

func closeSessionStep(closeFn func(context.Context) error) shutdownStep {
	return shutdownStep{name: "close message session", phase: phaseRemote, share: 1.0 / 6, fn: closeFn}
}

func deleteScaleSetStep(del func(context.Context) error) shutdownStep {
	return shutdownStep{name: "delete scale set", phase: phaseRemote, share: 1.0 / 4, fn: del}
}

// run executes the steps within budget, starting now.
func (s *shutdownSteps) run(budget time.Duration) {
	begin := time.Now()
	overall, cancel := context.WithDeadline(context.Background(), begin.Add(budget))
	defer cancel()
	local, cancelLocal := context.WithDeadline(overall, begin.Add(time.Duration(float64(budget)*(1-remoteShare))))
	defer cancelLocal()

	for _, phase := range []shutdownPhase{phaseQuiesce, phaseLocal, phaseRemote} {
		ctx := local
		if phase == phaseRemote {
			ctx = overall
		}
		var steps []shutdownStep
		for i := len(s.steps) - 1; i >= 0; i-- { // reverse registration, like defer
			if s.steps[i].phase == phase {
				steps = append(steps, s.steps[i])
			}
		}
		if phase == phaseLocal {
			var wg sync.WaitGroup
			for _, st := range steps {
				wg.Go(func() { s.runStep(ctx, budget, st) })
			}
			wg.Wait()
			continue
		}
		for _, st := range steps {
			s.runStep(ctx, budget, st)
		}
	}
}

func (s *shutdownSteps) runStep(ctx context.Context, budget time.Duration, st shutdownStep) {
	limit := budget // bounded by ctx's phase deadline anyway
	if st.share > 0 {
		limit = time.Duration(float64(budget) * st.share)
	}
	begin := time.Now()
	_, err := bounded(ctx, limit, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, st.fn(ctx)
	})
	switch {
	case err == nil:
		s.logger.Debug("Shutdown step done", slog.String("step", st.name), slog.Duration("took", time.Since(begin)))
	case ctx.Err() != nil || time.Since(begin) >= limit:
		s.logger.Warn("Shutdown step did not finish in time; abandoned", slog.String("step", st.name), slog.Duration("after", time.Since(begin)))
	default:
		s.logger.Warn("Shutdown step failed", slog.String("step", st.name), slog.String("error", err.Error()))
	}
}

// lifetimeClient wraps the session client given to the listener. scaleset
// v0.4.0's listener acknowledges messages and acquires jobs with
// cancellation stripped, so a stuck call would keep the listener, and with it
// shutdown, from ever starting. These calls are tied to the daemon's lifetime
// instead, with a per-call timeout, and abandoned when either ends.
type lifetimeClient struct {
	listener.Client
	life    context.Context
	timeout time.Duration
}

func (c lifetimeClient) DeleteMessage(_ context.Context, messageID int) error {
	_, err := bounded(c.life, c.timeout, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, c.Client.DeleteMessage(ctx, messageID)
	})
	return err
}

func (c lifetimeClient) AcquireJobs(_ context.Context, requestIDs []int64) ([]int64, error) {
	return bounded(c.life, c.timeout, func(ctx context.Context) ([]int64, error) {
		return c.Client.AcquireJobs(ctx, requestIDs)
	})
}

// GetMessage long-polls for up to ~50s; it keeps its own context, which the
// listener does cancel, but is also abandoned when the daemon stops.
func (c lifetimeClient) GetMessage(ctx context.Context, lastMessageID, maxCapacity int) (*scaleset.RunnerScaleSetMessage, error) {
	return bounded(ctx, 5*time.Minute, func(ctx context.Context) (*scaleset.RunnerScaleSetMessage, error) {
		return c.Client.GetMessage(ctx, lastMessageID, maxCapacity)
	})
}

// refreshWorker refreshes the runner image periodically until stopped.
type refreshWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// refreshTimeout bounds one refresh, including a slow image pull.
const refreshTimeout = 20 * time.Minute

func startRefreshWorker(parent context.Context, every time.Duration, refresh func(context.Context) error, logger *slog.Logger, current func() string) *refreshWorker {
	ctx, cancel := context.WithCancel(parent)
	w := &refreshWorker{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, rcancel := context.WithTimeout(ctx, refreshTimeout)
				err := refresh(rctx)
				rcancel()
				if err != nil && ctx.Err() == nil {
					logger.Error("Image refresh failed; new runners keep using the previous image",
						slog.String("image", current()), slog.String("error", err.Error()))
				}
			}
		}
	}()
	return w
}

// stop cancels the worker and waits for it, or for ctx.
func (w *refreshWorker) stop(ctx context.Context) error {
	w.cancel()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ listener.Client = lifetimeClient{}
