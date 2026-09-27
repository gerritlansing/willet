package microvm

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testProvisioner() *Provisioner {
	return &Provisioner{cfg: validConfig(), logger: slog.New(slog.DiscardHandler)}
}

// R01: a sandbox whose runner failed to launch, and which then couldn't be
// destroyed, must come back to the caller instead of being dropped.
func TestAbortStartReturnsUndestroyedVM(t *testing.T) {
	p := testProvisioner()
	// Graceful and forced destroy both fail once.
	sb := &fakeSandbox{errs: []error{errBoom, errBoom}}
	cause := errors.New("exec failed")

	vm, err := p.abortStart(context.Background(), "vm", sb, cause)
	if vm == nil {
		t.Fatal("undestroyed sandbox was dropped")
	}
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "will retry") {
		t.Fatalf("error should carry the cause and the teardown failure: %v", err)
	}
	select {
	case <-vm.Done():
	default:
		t.Fatal("returned VM should already count as exited")
	}
	if _, rerr := vm.Result(); !errors.Is(rerr, cause) {
		t.Fatalf("Result: %v", rerr)
	}
	if sb.closes != 0 {
		t.Fatal("handle closed although the sandbox still exists")
	}

	// The caller's retry goes through the normal VM.Destroy.
	if err := vm.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sb.closes != 1 {
		t.Fatalf("closes = %d after a successful destroy", sb.closes)
	}
}

func TestAbortStartDestroysWhenItCan(t *testing.T) {
	sb := &fakeSandbox{}
	vm, err := testProvisioner().abortStart(context.Background(), "vm", sb, errBoom)
	if vm != nil || !errors.Is(err, errBoom) {
		t.Fatalf("got vm=%v err=%v; want nil VM and the cause", vm, err)
	}
	if sb.destroys != 1 || sb.closes != 1 {
		t.Fatalf("destroys=%d closes=%d", sb.destroys, sb.closes)
	}
}

// R01: a temporary VM whose teardown failed blocks the next one until it is
// gone, keeping to the budget of one extra VM.
func TestTemporaryVMTeardownFailureIsKeptAndRetried(t *testing.T) {
	p := testProvisioner()
	stuck := &fakeSandbox{errs: []error{errBoom, errBoom, errBoom, errBoom}}

	if err := p.releaseAux(context.Background(), "pull", stuck); err == nil {
		t.Fatal("teardown failure was not reported")
	}
	if p.pendingAux != stuck {
		t.Fatal("undestroyed temporary VM was not remembered")
	}

	// Next temporary VM: the retry fails again, so it must not be created.
	if err := p.clearPendingAux(context.Background()); err == nil {
		t.Fatal("a second temporary VM would be admitted while one still exists")
	}
	if p.pendingAux != stuck {
		t.Fatal("pending VM forgotten after a failed retry")
	}

	// Shutdown's Close retries; now it succeeds.
	if err := p.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.pendingAux != nil || stuck.closes != 1 {
		t.Fatalf("pending=%v closes=%d after a successful retry", p.pendingAux, stuck.closes)
	}
	if err := p.clearPendingAux(context.Background()); err != nil {
		t.Fatalf("nothing pending, but: %v", err)
	}
}

// M02: withTempVM owns the whole temporary-VM lifecycle and reports teardown
// failures even when the operation itself failed.
func TestWithTempVMErrors(t *testing.T) {
	errUse := errors.New("probe failed")
	failTeardown := func() *fakeSandbox { return &fakeSandbox{errs: []error{errBoom, errBoom}} } // graceful + forced

	t.Run("operation fails, teardown succeeds", func(t *testing.T) {
		p, sb := testProvisioner(), &fakeSandbox{}
		err := withTempVM(context.Background(), p, "tmp", func() (*fakeSandbox, error) { return sb, nil }, func(*fakeSandbox) error { return errUse })
		if !errors.Is(err, errUse) || errors.Is(err, errBoom) {
			t.Fatalf("got %v; want only the operation error", err)
		}
		if p.pendingAux != nil || sb.closes != 1 {
			t.Fatal("VM not released")
		}
	})

	t.Run("operation succeeds, teardown fails", func(t *testing.T) {
		p, sb := testProvisioner(), failTeardown()
		err := withTempVM(context.Background(), p, "tmp", func() (*fakeSandbox, error) { return sb, nil }, func(*fakeSandbox) error { return nil })
		if !errors.Is(err, errBoom) {
			t.Fatalf("got %v; want the teardown error", err)
		}
		if p.pendingAux != sb {
			t.Fatal("undestroyed VM not kept for retry")
		}
	})

	t.Run("both fail", func(t *testing.T) {
		p, sb := testProvisioner(), failTeardown()
		err := withTempVM(context.Background(), p, "tmp", func() (*fakeSandbox, error) { return sb, nil }, func(*fakeSandbox) error { return errUse })
		if !errors.Is(err, errUse) || !errors.Is(err, errBoom) {
			t.Fatalf("got %v; want both the operation and the teardown error", err)
		}
		if p.pendingAux != sb {
			t.Fatal("undestroyed VM not kept for retry")
		}

		// No second temporary VM while the first still exists.
		sb.errs = []error{errBoom, errBoom}
		created := false
		err = withTempVM(context.Background(), p, "tmp2", func() (*fakeSandbox, error) { created = true; return &fakeSandbox{}, nil }, func(*fakeSandbox) error { return nil })
		if err == nil || created {
			t.Fatalf("second temporary VM admitted (created=%v, err=%v)", created, err)
		}

		// Once the leftover is destroyed, the next one proceeds.
		err = withTempVM(context.Background(), p, "tmp3", func() (*fakeSandbox, error) { created = true; return &fakeSandbox{}, nil }, func(*fakeSandbox) error { return nil })
		if err != nil || !created || p.pendingAux != nil {
			t.Fatalf("after the leftover was destroyed: created=%v err=%v pending=%v", created, err, p.pendingAux)
		}
	})

	t.Run("create fails", func(t *testing.T) {
		p := testProvisioner()
		used := false
		err := withTempVM(context.Background(), p, "tmp", func() (*fakeSandbox, error) { return nil, errUse }, func(*fakeSandbox) error { used = true; return nil })
		if !errors.Is(err, errUse) || used || p.pendingAux != nil {
			t.Fatalf("err=%v used=%v pending=%v", err, used, p.pendingAux)
		}
	})
}

// A pending sandbox whose creation hasn't finished can't be torn down yet,
// and says so, instead of reporting success.
func TestPendingSandboxWhileCreating(t *testing.T) {
	c := &creation{name: "vm", done: make(chan struct{})}
	pending := &pendingSandbox{p: testProvisioner(), c: c}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := pending.Destroy(ctx); !errors.Is(err, errStillCreating) {
		t.Fatalf("got %v; want errStillCreating", err)
	}
	if err := pending.Close(); err != nil {
		t.Fatalf("Close before the creation finished: %v", err)
	}
}
