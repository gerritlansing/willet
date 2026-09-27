package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
	"github.com/google/uuid"
)

var quiet = slog.New(slog.DiscardHandler)

func TestBoundedAbandonsCallsThatIgnoreContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	begin := time.Now()
	_, err := bounded(context.Background(), 50*time.Millisecond, func(context.Context) (int, error) {
		<-release // like a call queued on scaleset's mutex
		return 1, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(begin) > time.Second {
		t.Fatalf("got %v after %s; want a timeout at ~50ms", err, time.Since(begin))
	}

	parent, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if _, err := bounded(parent, time.Hour, func(context.Context) (int, error) { <-release; return 0, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("parent cancel: %v", err)
	}

	v, err := bounded(context.Background(), time.Second, func(context.Context) (int, error) { return 7, nil })
	if v != 7 || err != nil {
		t.Fatalf("normal call: %d %v", v, err)
	}
}

func TestShutdownStepAbandonedAtItsShare(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var after atomic.Bool
	var s shutdownSteps
	s.logger = quiet
	s.add(closeSessionStep(func(context.Context) error { after.Store(true); return nil }))
	s.add(stopRefreshStep(func(context.Context) error { <-release; return nil })) // stuck, ignores ctx

	begin := time.Now()
	s.run(600 * time.Millisecond)
	if d := time.Since(begin); d > 400*time.Millisecond {
		t.Fatalf("took %s; the stuck step should be abandoned at 1/6 of the budget", d)
	}
	if !after.Load() {
		t.Fatal("steps after an abandoned one did not run")
	}
}

func TestShutdownStepsRespectOverallBudget(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	stuck := func(context.Context) error { <-release; return nil }
	var s shutdownSteps
	s.logger = quiet
	s.add(destroyTempVMStep(stuck))
	s.add(deleteScaleSetStep(stuck))
	s.add(closeSessionStep(stuck))
	s.add(stopRunnersStep(func(context.Context) { <-release }))
	s.add(stopRefreshStep(stuck))
	begin := time.Now()
	s.run(200 * time.Millisecond)
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("shutdown took %s with a 200ms budget", d)
	}
}

// V01 (third review): local VM teardown, including a leftover temporary VM,
// must run before GitHub cleanup and get a usable share of the budget, even
// when everything else is slow. Steps are registered in the order run()
// registers them, using the same constructors.
func TestShutdownPlanLocalTeardownFirst(t *testing.T) {
	for _, deleteOnExit := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleteOnExit=%v", deleteOnExit), func(t *testing.T) {
			const budget = 600 * time.Millisecond
			release := make(chan struct{})
			defer close(release)

			var mu sync.Mutex
			var events []string
			note := func(e string) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, e)
			}
			var tempRemaining time.Duration
			var tempCtxErr error

			var s shutdownSteps
			s.logger = quiet
			// Registration order as in run():
			s.add(destroyTempVMStep(func(ctx context.Context) error {
				dl, _ := ctx.Deadline()
				tempRemaining, tempCtxErr = time.Until(dl), ctx.Err()
				note("temp VM")
				return nil
			}))
			if deleteOnExit {
				s.add(deleteScaleSetStep(func(context.Context) error { note("delete scale set"); <-release; return nil }))
			}
			s.add(closeSessionStep(func(context.Context) error { note("close session"); <-release; return nil }))
			s.add(stopRunnersStep(func(ctx context.Context) { note("runners"); <-ctx.Done() }))
			s.add(stopRefreshStep(func(ctx context.Context) error { note("refresh"); <-ctx.Done(); return ctx.Err() }))

			begin := time.Now()
			s.run(budget)
			if d := time.Since(begin); d > budget+150*time.Millisecond {
				t.Fatalf("shutdown took %s with a %s budget", d, budget)
			}
			if tempCtxErr != nil {
				t.Fatalf("temporary VM cleanup got a dead context: %v", tempCtxErr)
			}
			// Local phase ends at 75% of the budget; refresh may use 1/6.
			if min := budget / 3; tempRemaining < min {
				t.Fatalf("temporary VM cleanup got %s, want at least %s", tempRemaining, min)
			}
			mu.Lock()
			defer mu.Unlock()
			pos := func(e string) int { return slices.Index(events, e) }
			if pos("refresh") != 0 {
				t.Errorf("image refresh must stop first: %v", events)
			}
			if pos("temp VM") > pos("close session") || pos("runners") > pos("close session") {
				t.Errorf("local teardown must come before GitHub cleanup: %v", events)
			}
			if deleteOnExit && pos("delete scale set") < pos("close session") {
				t.Errorf("scale set deleted before the session was closed: %v", events)
			}
			if !deleteOnExit && pos("delete scale set") >= 0 {
				t.Errorf("scale set deleted without --delete-on-exit: %v", events)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// R05: a stuck message acknowledgement must not keep the listener, and with
// it shutdown, from starting.

type stuckSession struct {
	release chan struct{}
	sent    atomic.Bool
	acked   atomic.Bool
}

func (s *stuckSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	if s.sent.CompareAndSwap(false, true) {
		return &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// DeleteMessage ignores its context entirely, like a call waiting on the
// scaleset client's mutex.
func (s *stuckSession) DeleteMessage(context.Context, int) error {
	s.acked.Store(true)
	<-s.release
	return nil
}

func (s *stuckSession) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }
func (s *stuckSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

type nopScaler struct{}

func (nopScaler) HandleJobStarted(context.Context, *scaleset.JobStarted) error     { return nil }
func (nopScaler) HandleJobCompleted(context.Context, *scaleset.JobCompleted) error { return nil }
func (nopScaler) HandleDesiredRunnerCount(_ context.Context, n int) (int, error)   { return n, nil }

func TestListenerReturnsDespiteStuckAcknowledgement(t *testing.T) {
	session := &stuckSession{release: make(chan struct{})}
	defer close(session.release)
	life, cancel := context.WithCancel(context.Background())
	defer cancel()

	l, err := listener.New(lifetimeClient{Client: session, life: life, timeout: time.Hour}, listener.Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Run(life, nopScaler{}) }()

	waitFor(t, 2*time.Second, "listener to acknowledge the message", session.acked.Load)

	cancel() // SIGTERM while the acknowledgement is stuck
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener did not return while an acknowledgement was stuck; shutdown would never start")
	}
}

// Without the wrapper the pinned listener stays blocked on a stuck
// acknowledgement: this documents why lifetimeClient exists. If a scaleset
// upgrade fixes that, the test logs a note rather than failing, since the
// wrapper stays harmless. Both outcomes must terminate (review finding M01).
func TestUnwrappedListenerOnStuckAcknowledgement(t *testing.T) {
	t.Run("pinned SDK blocks", func(t *testing.T) {
		returnedEarly := runUnwrappedListener(t, &stuckSession{release: make(chan struct{})})
		if returnedEarly {
			t.Log("the listener now honours cancellation for acknowledgements; lifetimeClient may no longer be needed")
		}
	})
	t.Run("fixed SDK returns promptly", func(t *testing.T) {
		// Simulates an SDK that no longer blocks: the acknowledgement
		// completes, so the listener's next long poll sees the cancellation.
		s := &stuckSession{release: make(chan struct{})}
		close(s.release)
		if !runUnwrappedListener(t, s) {
			t.Fatal("listener did not return promptly although nothing was stuck")
		}
	})
}

// runUnwrappedListener runs the pinned listener directly on session, cancels
// it once the message is being acknowledged, and reports whether it returned
// before the acknowledgement was released. It always waits for the listener
// to finish, within a bound.
func runUnwrappedListener(t *testing.T, session *stuckSession) (returnedEarly bool) {
	t.Helper()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			select {
			case <-session.release:
			default:
				close(session.release)
			}
		})
	}
	t.Cleanup(release)

	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	l, err := listener.New(session, listener.Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = l.Run(life, nopScaler{})
	}()

	waitFor(t, 2*time.Second, "listener to acknowledge the message", session.acked.Load)
	cancel()
	select {
	case <-finished:
		returnedEarly = true
	case <-time.After(200 * time.Millisecond):
	}
	release()
	within(t, 2*time.Second, "listener to return after the acknowledgement was released", finished)
	return returnedEarly
}

// waitFor polls cond until it holds, failing the test after limit.
func waitFor(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", limit, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// within waits for ch to be closed or receive, failing the test after limit.
func within(t *testing.T, limit time.Duration, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(limit):
		t.Fatalf("timed out after %s waiting for %s", limit, what)
	}
}

// ---------------------------------------------------------------------------
// R04: the refresh worker can be stopped and waited for within a deadline.

func TestRefreshWorkerStops(t *testing.T) {
	var calls atomic.Int32
	w := startRefreshWorker(context.Background(), 10*time.Millisecond, func(context.Context) error {
		calls.Add(1)
		return nil
	}, quiet, func() string { return "img" })
	waitFor(t, 2*time.Second, "two refreshes", func() bool { return calls.Load() >= 2 })
	stopCtx, cancelStop := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStop()
	if err := w.stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	n := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != n {
		t.Fatal("worker kept refreshing after stop returned")
	}
}

func TestRefreshWorkerStopIsBoundedWhenRefreshIsStuck(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{})
	var once sync.Once
	w := startRefreshWorker(context.Background(), 5*time.Millisecond, func(context.Context) error {
		once.Do(func() { close(started) })
		<-release // ignores cancellation
		return nil
	}, quiet, func() string { return "img" })
	within(t, 2*time.Second, "the first refresh to start", started)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if err := w.stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop: %v", err)
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("stop took %s past its deadline", d)
	}
}
