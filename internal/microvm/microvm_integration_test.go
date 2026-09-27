//go:build integration

package microvm

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	msb "github.com/superradcompany/microsandbox/sdk/go"
)

const testOwner = "it0000000001"

func testConfig() Config {
	return Config{
		OwnerID:      testOwner,
		Image:        "ghcr.io/actions/actions-runner:latest",
		CPUs:         1,
		MemoryMiB:    1024,
		User:         "runner",
		RunnerDir:    "/home/runner",
		ScaleSetName: "willet-it",
	}
}

func newProv(t *testing.T) *Provisioner {
	t.Helper()
	return newProvFor(t, testOwner)
}

func newProvFor(t *testing.T, ownerID string) *Provisioner {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cfg := testConfig()
	cfg.OwnerID = ownerID
	p, err := New(t.Context(), cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.CleanupOrphans(context.Background()) })
	return p
}

func countOwned(t *testing.T) int {
	t.Helper()
	return countOwnedBy(t, testOwner)
}

func countOwnedBy(t *testing.T, ownerID string) int {
	t.Helper()
	page, err := msb.ListSandboxesWith(t.Context(), msb.WithListLabels(map[string]string{LabelOwner: ownerID}))
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Sandboxes)
}

func TestRefreshWithRealRunnerImage(t *testing.T) {
	p := newProv(t)
	res, err := p.Refresh(t.Context(), "willet-it-refresh")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pinned %s, runner %s", res.Ref, res.RunnerVersion)
	if !strings.HasPrefix(res.Ref, "ghcr.io/actions/actions-runner@sha256:") || res.RunnerVersion == "" {
		t.Fatalf("unexpected result %+v", res)
	}
	if n := countOwned(t); n != 0 {
		t.Fatalf("%d sandboxes left after refresh", n)
	}

	bad := newProv(t)
	bad.cfg.RunnerDir = "/nonexistent"
	if _, err := bad.Refresh(t.Context(), "willet-it-refresh"); err == nil {
		t.Fatal("refresh accepted an image without run.sh")
	}
	bad.cfg.RunnerDir = "/home/runner"
	bad.cfg.User = "no-such-user"
	if _, err := bad.Refresh(t.Context(), "willet-it-refresh"); err == nil {
		t.Fatal("refresh accepted a runner user that does not exist")
	}
}

// With a bogus JIT config the real runner starts, rejects it and exits on its
// own; the exit must be observed. Note run.sh exits 0 even in this case, so
// the scaler must not rely on exit codes to detect failed runners.
func TestRunnerExitIsObserved(t *testing.T) {
	p := newProv(t)
	var logs syncBuffer
	p.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	jit := base64.StdEncoding.EncodeToString([]byte(`{"bogus":"config"}`))

	vm, err := p.Start(t.Context(), "willet-it-exit", jit)
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Destroy(context.Background())

	select {
	case <-vm.Done():
	case <-time.After(2 * time.Minute):
		t.Fatal("runner did not exit")
	}
	code, err := vm.Result()
	t.Logf("runner exit code %d, err %v", code, err)
	// Runner output is logged only when RunnerOutput is set.
	if strings.Contains(logs.String(), "stream=") {
		t.Fatalf("runner output logged although RunnerOutput is off:\n%s", logs.String())
	}

	if err := vm.Destroy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := countOwned(t); n != 0 {
		t.Fatalf("%d sandboxes left after destroy", n)
	}
}

func TestDestroyStopsRunningRunner(t *testing.T) {
	p := newProv(t)
	// A fake run.sh that, like a real idle runner, never exits by itself.
	mode := uint32(0o755)
	p.cfg.RunnerDir = "/opt/fake-runner"
	p.extra = []msb.SandboxOption{msb.WithPatches(
		msb.Patch.Mkdir("/opt/fake-runner", msb.PatchOptions{}),
		msb.Patch.Text("/opt/fake-runner/run.sh", "#!/bin/sh\necho \"jit=${ACTIONS_RUNNER_INPUT_JITCONFIG}\"\nexec sleep 3600\n", msb.PatchOptions{Mode: &mode}),
	)}

	vm, err := p.Start(t.Context(), "willet-it-long", "secret-jit")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
		code, err := vm.Result()
		t.Fatalf("runner exited early: code %d err %v", code, err)
	case <-time.After(3 * time.Second):
	}

	start := time.Now()
	if err := vm.Destroy(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-vm.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Done did not fire after Destroy")
	}
	t.Logf("destroy took %s", time.Since(start))
	if n := countOwned(t); n != 0 {
		t.Fatalf("%d sandboxes left after destroy", n)
	}
}

func TestCleanupOrphans(t *testing.T) {
	p := newProv(t)
	for _, name := range []string{"willet-it-orphan-1", "willet-it-orphan-2"} {
		sb, err := msb.CreateSandbox(t.Context(), name, append(p.sandboxOptions(name, p.cfg.Image), msb.WithDetached())...)
		if err != nil {
			t.Fatal(err)
		}
		_ = sb.Detach(t.Context())
	}
	if n := countOwned(t); n != 2 {
		t.Fatalf("expected 2 orphans, got %d", n)
	}
	if err := p.CleanupOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := countOwned(t); n != 0 {
		t.Fatalf("%d orphans left", n)
	}
}

// Two daemons whose scale sets share a name (e.g. "msb" in two orgs) must
// never clean up each other's VMs.
func TestCleanupOrphansSparesOtherOwners(t *testing.T) {
	mine := newProvFor(t, "it00000000aa")
	theirs := newProvFor(t, "it00000000bb")

	boot := func(p *Provisioner, name string) {
		sb, err := msb.CreateSandbox(t.Context(), name, append(p.sandboxOptions(name, p.cfg.Image), msb.WithDetached())...)
		if err != nil {
			t.Fatal(err)
		}
		_ = sb.Detach(t.Context())
	}
	boot(mine, "willet-it-mine")
	boot(theirs, "willet-it-theirs")

	if err := mine.CleanupOrphans(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := countOwnedBy(t, "it00000000aa"); n != 0 {
		t.Fatalf("%d of my orphans left", n)
	}
	if n := countOwnedBy(t, "it00000000bb"); n != 1 {
		t.Fatalf("other owner's VM was touched: %d left, want 1", n)
	}
}

// Cancelling a start while its sandbox is being created must not leave a
// sandbox behind (microsandbox#1687). The caller may get back a pending VM,
// whose teardown then waits for the creation and removes the result.
func TestCanceledStartLeavesNothing(t *testing.T) {
	const owner = "it0000000300"
	p := newProvFor(t, owner)
	if _, err := p.Refresh(t.Context(), "willet-it-cancel-refresh"); err != nil {
		t.Fatal(err)
	}
	canceled := 0
	for i, delay := range []time.Duration{20, 60, 100, 140, 180, 220, 260} {
		name := fmt.Sprintf("willet-it-cancel-%d", i)
		ctx, cancel := context.WithTimeout(context.Background(), delay*time.Millisecond)
		vm, err := p.Start(ctx, name, "jit")
		cancel()
		if err == nil {
			// Finished before the deadline.
			if derr := vm.Destroy(context.Background()); derr != nil {
				t.Fatalf("delay %s: teardown of a started VM: %v", delay*time.Millisecond, derr)
			}
			continue
		}
		canceled++
		if vm != nil {
			dctx, dcancel := context.WithTimeout(context.Background(), time.Minute)
			derr := vm.Destroy(dctx)
			dcancel()
			if derr != nil {
				t.Fatalf("delay %s: teardown of the pending VM: %v", delay*time.Millisecond, derr)
			}
		}
		// Give any background rollback time to surface, then look for it.
		time.Sleep(500 * time.Millisecond)
		if h, gerr := msb.GetSandbox(context.Background(), name); !msb.IsKind(gerr, msb.ErrSandboxNotFound) {
			if gerr == nil {
				t.Fatalf("delay %s: canceled start left sandbox %s (%s)", delay*time.Millisecond, name, h.Status())
			}
			t.Fatalf("delay %s: lookup failed, so absence is unproven: %v", delay*time.Millisecond, gerr)
		}
	}
	if canceled == 0 {
		t.Fatal("no start was canceled during creation; the test proves nothing")
	}
	t.Logf("%d starts canceled during creation, none left anything behind", canceled)
	if n := countOwnedBy(t, owner); n != 0 {
		t.Fatalf("%d sandboxes left", n)
	}
}
