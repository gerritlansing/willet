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
		<-release // a call that ignores its context
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
// The acknowledgement must stay tied to a stop request even though the
// listener strips cancellation from it.

// ackSession serves one message, then waits. DeleteMessage blocks until its
// context ends, like a request stalled on a dead connection.
type ackSession struct {
	sent      atomic.Bool
	acking    chan struct{}
	ackResult chan error
}

func newAckSession() *ackSession {
	return &ackSession{acking: make(chan struct{}), ackResult: make(chan error, 1)}
}

func (s *ackSession) GetMessage(ctx context.Context, _, _ int) (*scaleset.RunnerScaleSetMessage, error) {
	if s.sent.CompareAndSwap(false, true) {
		return &scaleset.RunnerScaleSetMessage{MessageID: 1, Statistics: &scaleset.RunnerScaleSetStatistic{}}, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (s *ackSession) DeleteMessage(ctx context.Context, _ int) error {
	close(s.acking)
	<-ctx.Done()
	s.ackResult <- ctx.Err()
	return ctx.Err()
}

func (s *ackSession) AcquireJobs(context.Context, []int64) ([]int64, error) { return nil, nil }
func (s *ackSession) Session() scaleset.RunnerScaleSetSession {
	return scaleset.RunnerScaleSetSession{SessionID: uuid.New(), Statistics: &scaleset.RunnerScaleSetStatistic{}}
}

type nopScaler struct{}

func (nopScaler) Scale(context.Context, *scaleset.RunnerScaleSetMessage) error { return nil }

func runListener(t *testing.T, client listener.Client, drain context.Context) <-chan error {
	t.Helper()
	l, err := listener.New(client, listener.Config{ScaleSetID: 1, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Run(drain, nopScaler{}) }()
	return done
}

func TestForceStopInterruptsAStalledAcknowledgement(t *testing.T) {
	session := newAckSession()
	force, cancelForce := context.WithCancel(context.Background())
	defer cancelForce()
	drain, cancelDrain := context.WithCancel(force)
	defer cancelDrain()

	done := runListener(t, ackClient{Client: session, life: force, timeout: time.Hour}, drain)
	within(t, 2*time.Second, "the listener to start acknowledging", session.acking)

	// The first signal starts a drain. The listener acks regardless, so a
	// stalled ack keeps Run from returning.
	cancelDrain()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v while the acknowledgement was stalled", err)
	case <-time.After(100 * time.Millisecond):
	}

	// The second signal must get it unstuck.
	cancelForce()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned nil although its acknowledgement was cut off")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a force stop did not interrupt the stalled acknowledgement")
	}
	if err := <-session.ackResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("the acknowledgement's context ended with %v, want canceled", err)
	}
}

func TestStalledAcknowledgementHasADeadline(t *testing.T) {
	session := newAckSession()
	done := runListener(t, ackClient{Client: session, life: context.Background(), timeout: 50 * time.Millisecond}, context.Background())
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a stalled acknowledgement was never given up on")
	}
	if err := <-session.ackResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the acknowledgement's context ended with %v, want a deadline", err)
	}
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
