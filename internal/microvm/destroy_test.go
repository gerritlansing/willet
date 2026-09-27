package microvm

import (
	"context"
	"errors"
	"sync"
	"testing"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

// fakeSandbox fails its first len(errs) Destroy calls with the queued errors.
// Each VM.Destroy makes a graceful call, then a forced one if that fails.
type fakeSandbox struct {
	mu       sync.Mutex
	errs     []error
	destroys int
	closes   int
}

func (f *fakeSandbox) Destroy(ctx context.Context, _ ...msb.DestroyOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.destroys++
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return nil
}

func (f *fakeSandbox) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func newTestVM(sb sandbox) *VM {
	return &VM{name: "vm", sb: sb, done: make(chan struct{})}
}

var errBoom = errors.New("boom")

func TestDestroyRetriesAfterFailure(t *testing.T) {
	// Graceful and forced both fail on the first call; the second call succeeds.
	sb := &fakeSandbox{errs: []error{errBoom, errBoom}}
	vm := newTestVM(sb)

	if err := vm.Destroy(context.Background()); !errors.Is(err, errBoom) {
		t.Fatalf("first call: want the failure, got %v", err)
	}
	if sb.closes != 0 {
		t.Fatal("handle was closed after a failed destroy")
	}
	if sb.destroys != 2 {
		t.Fatalf("want graceful then forced attempt, got %d calls", sb.destroys)
	}

	if err := vm.Destroy(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if sb.destroys != 3 {
		t.Fatalf("second call must make a real attempt: %d destroy calls, want 3", sb.destroys)
	}
	if sb.closes != 1 {
		t.Fatalf("handle closes = %d, want 1", sb.closes)
	}

	// Already destroyed: no further runtime calls.
	if err := vm.Destroy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sb.destroys != 3 {
		t.Fatalf("destroy after success reached the runtime again")
	}
}

func TestDestroyForcedFallbackSucceeds(t *testing.T) {
	sb := &fakeSandbox{errs: []error{errBoom}}
	if err := newTestVM(sb).Destroy(context.Background()); err != nil {
		t.Fatalf("forced fallback should have succeeded: %v", err)
	}
	if sb.destroys != 2 || sb.closes != 1 {
		t.Fatalf("destroys=%d closes=%d, want 2 (graceful, forced) and 1", sb.destroys, sb.closes)
	}
}

func TestDestroyTreatsNotFoundAsSuccess(t *testing.T) {
	sb := &fakeSandbox{errs: []error{&msb.Error{Kind: msb.ErrSandboxNotFound}}}
	if err := newTestVM(sb).Destroy(context.Background()); err != nil {
		t.Fatalf("not found means already gone: %v", err)
	}
}

func TestDestroyCanceledContextIsNotSuccess(t *testing.T) {
	sb := &fakeSandbox{}
	vm := newTestVM(sb)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := vm.Destroy(ctx); err == nil {
		t.Fatal("destroy with a canceled context reported success")
	}
	if err := vm.Destroy(context.Background()); err != nil {
		t.Fatalf("retry with a live context: %v", err)
	}
}

func TestDestroyConcurrentCallers(t *testing.T) {
	sb := &fakeSandbox{}
	vm := newTestVM(sb)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := vm.Destroy(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if sb.destroys != 1 || sb.closes != 1 {
		t.Fatalf("destroys=%d closes=%d, want exactly one of each", sb.destroys, sb.closes)
	}
}
