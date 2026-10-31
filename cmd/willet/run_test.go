package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gerritlansing/willet/internal/owner"
)

func lockTestConfig() Config {
	return Config{
		RegistrationURL:      "https://github.com/org",
		ScaleSetName:         "msb",
		RunnerGroup:          "default",
		Token:                "ghp_synthetic",
		RunnerImage:          "ghcr.io/actions/actions-runner:latest",
		CPUs:                 1,
		MemoryMiB:            1024,
		RunnerUser:           "runner",
		RunnerDir:            "/home/runner",
		DNSRebindProtection:  true,
		StartTimeout:         time.Minute,
		ImageRefreshInterval: 24 * time.Hour,
		LogFormat:            "json",
		LogLevel:             "error",
	}
}

// A duplicate daemon must stop at the lock, before runtime setup, orphan
// cleanup or any GitHub call. The context is canceled, so any step after the
// lock would fail with context.Canceled instead of ErrLocked.
func TestRunStopsAtLockBeforeAnyOtherStep(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", stateDir)
	cfg := lockTestConfig()

	id, err := owner.ID(cfg.RegistrationURL, cfg.RunnerGroup, cfg.ScaleSetName)
	if err != nil {
		t.Fatal(err)
	}
	held, err := owner.Acquire(stateDir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, ctx, cfg); !errors.Is(err, owner.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}

	// Control: with the lock free, run gets past it and fails later, on the
	// canceled context. This proves the check above isn't vacuous.
	held.Release()
	if err := run(ctx, ctx, cfg); err == nil || errors.Is(err, owner.ErrLocked) {
		t.Fatalf("with the lock free, want a later failure, got %v", err)
	}
}

// Same display name, different scope: separate identities, separate locks.
func TestRunLockIsPerScaleSetIdentity(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("STATE_DIRECTORY", stateDir)
	other := lockTestConfig()
	other.RegistrationURL = "https://github.com/another-org"

	id, err := owner.ID(other.RegistrationURL, other.RunnerGroup, other.ScaleSetName)
	if err != nil {
		t.Fatal(err)
	}
	held, err := owner.Acquire(stateDir, id)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, ctx, lockTestConfig()); errors.Is(err, owner.ErrLocked) {
		t.Fatalf("a different org's lock blocked this scale set: %v", err)
	}
}

func TestHandleSignals(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	drain, force, stop := handleSignals(context.Background(), sigs)
	defer stop()

	if drain.Err() != nil || force.Err() != nil {
		t.Fatal("contexts canceled before any signal")
	}
	sigs <- syscall.SIGTERM
	<-drain.Done()
	if force.Err() != nil {
		t.Fatal("first signal must drain, not force")
	}
	sigs <- syscall.SIGINT
	select {
	case <-force.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("second signal did not force")
	}
}

func TestHandleSignalsParentCancelStopsBoth(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	drain, force, stop := handleSignals(parent, make(chan os.Signal))
	defer stop()
	cancel()
	<-drain.Done()
	<-force.Done()
}

func TestDrainRunners(t *testing.T) {
	ok := func(context.Context) error { return nil }
	cases := []struct {
		name   string
		force  func() (context.Context, context.CancelFunc)
		drain  func(context.Context) error
		wantIn string
	}{
		{name: "drained", force: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, drain: ok, wantIn: "Drained"},
		{name: "timeout", force: func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			drain: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, wantIn: "Drain timeout reached"},
		{name: "forced", force: func() (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, cancel
		}, drain: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }, wantIn: "Second stop request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf strings.Builder
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			force, cancel := c.force()
			defer cancel()
			var closed atomic.Int32
			drainRunners(force, logger, 30*time.Millisecond, func(context.Context) error { closed.Add(1); return nil }, c.drain)
			deadline := time.Now().Add(time.Second) // an abandoned close may still be starting
			for closed.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if got := closed.Load(); got != 1 {
				t.Fatalf("session closed %d times, want 1", got)
			}
			if !strings.Contains(buf.String(), c.wantIn) {
				t.Fatalf("log lacks %q:\n%s", c.wantIn, buf.String())
			}
		})
	}
}

func TestRetryUntilClosed(t *testing.T) {
	calls := 0
	fail := true
	c := retryUntilClosed(func(context.Context) error {
		calls++
		if fail {
			return errors.New("boom")
		}
		return nil
	})
	if c(context.Background()) == nil {
		t.Fatal("error swallowed")
	}
	fail = false
	for range 3 {
		if err := c(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("closeFn called %d times, want 2 (one failure, one success)", calls)
	}
}

func TestParseSystemdSpan(t *testing.T) {
	cases := []struct {
		in       string
		want     time.Duration
		infinite bool
		bad      bool
	}{
		{in: "1min 30s", want: 90 * time.Second},
		{in: "6h 2min", want: 6*time.Hour + 2*time.Minute},
		{in: "1d", want: 24 * time.Hour},
		{in: "500ms", want: 500 * time.Millisecond},
		{in: "infinity", infinite: true},
		{in: "", bad: true},
		{in: "soon", bad: true},
	}
	for _, c := range cases {
		d, inf, err := parseSystemdSpan(c.in)
		if (err != nil) != c.bad || d != c.want || inf != c.infinite {
			t.Errorf("parseSystemdSpan(%q) = %v, %v, %v", c.in, d, inf, err)
		}
	}
}

func TestUnitWarnings(t *testing.T) {
	cases := []struct {
		show string
		want int
	}{
		{"KillMode=mixed\nTimeoutStopUSec=6h 2min\n", 0},
		{"KillMode=mixed\nTimeoutStopUSec=infinity\n", 0},
		{"KillMode=process\nTimeoutStopUSec=1d\n", 0},
		{"KillMode=control-group\nTimeoutStopUSec=6h 2min\n", 1},
		{"KillMode=mixed\nTimeoutStopUSec=1min 30s\n", 1},
		{"KillMode=control-group\nTimeoutStopUSec=1min 30s\n", 2},
	}
	for _, c := range cases {
		if got := unitWarnings(c.show, "willet"); len(got) != c.want {
			t.Errorf("unitWarnings(%q) = %v, want %d warnings", c.show, got, c.want)
		}
	}
}
