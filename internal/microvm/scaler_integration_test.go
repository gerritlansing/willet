//go:build integration

package microvm

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
	msb "github.com/superradcompany/microsandbox/sdk/go"

	"github.com/gerritlansing/willet/internal/scaler"
)

// fakeGitHub hands out JIT configs and records deregistrations.
type fakeGitHub struct {
	mu      sync.Mutex
	next    int
	removed map[int64]bool
}

func (f *fakeGitHub) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, _ int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	return &scaleset.RunnerScaleSetJitRunnerConfig{Runner: &scaleset.RunnerReference{ID: f.next, Name: s.Name}, EncodedJITConfig: "jit"}, nil
}

func (f *fakeGitHub) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[id] = true
	return nil
}

type vmProvisioner struct{ *Provisioner }

func (p vmProvisioner) Start(ctx context.Context, name, jit string) (scaler.RunnerVM, error) {
	vm, err := p.Provisioner.Start(ctx, name, jit)
	if vm == nil {
		return nil, err
	}
	return vm, err
}

// The real scaler on real VMs: capacity is respected, a completed job's
// lingering VM is reclaimed, and shutdown leaves nothing behind.
func TestScalerWithRealVMs(t *testing.T) {
	p := newProvFor(t, "it00000000cc")
	mode := uint32(0o755)
	p.cfg.RunnerDir = "/opt/fake-runner"
	p.extra = []msb.SandboxOption{msb.WithPatches(
		msb.Patch.Mkdir("/opt/fake-runner", msb.PatchOptions{}),
		msb.Patch.Text("/opt/fake-runner/run.sh", "#!/bin/sh\nexec sleep 3600\n", msb.PatchOptions{Mode: &mode}),
	)}

	gh := &fakeGitHub{removed: map[int64]bool{}}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := scaler.New(t.Context(), scaler.Config{
		ScaleSetID: 1, NamePrefix: "willet-it", MaxRunners: 2,
		CompletionGrace: 2 * time.Second,
	}, gh, vmProvisioner{p}, logger)

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(time.Minute)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	owned := func() int { return countOwnedBy(t, "it00000000cc") }

	// Demand above the cap starts exactly MaxRunners VMs.
	if _, err := s.HandleDesiredRunnerCount(t.Context(), 5); err != nil {
		t.Fatal(err)
	}
	waitFor("2 VMs", func() bool { return owned() == 2 })

	// Complete a job on one runner. Its process never exits, so the grace
	// period expires and the scaler destroys the VM; the cap holds throughout.
	page, err := msb.ListSandboxesWith(t.Context(), msb.WithListLabels(map[string]string{LabelOwner: "it00000000cc"}))
	if err != nil {
		t.Fatal(err)
	}
	name := page.Sandboxes[0].Name()
	_ = s.HandleJobStarted(t.Context(), &scaleset.JobStarted{RunnerName: name})
	_ = s.HandleJobCompleted(t.Context(), &scaleset.JobCompleted{RunnerName: name})
	for range 10 {
		_, _ = s.HandleDesiredRunnerCount(t.Context(), 5)
		if n := owned(); n > 2 {
			t.Fatalf("%d VMs with max-runners 2", n)
		}
		time.Sleep(300 * time.Millisecond)
	}
	waitFor("lingering VM destroyed", func() bool {
		_, err := msb.GetSandbox(t.Context(), name)
		return msb.IsKind(err, msb.ErrSandboxNotFound)
	})

	// Its slot is reused.
	_, _ = s.HandleDesiredRunnerCount(t.Context(), 5)
	waitFor("replacement VM", func() bool { return owned() == 2 })

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	begin := time.Now()
	s.Shutdown(ctx)
	t.Logf("shutdown took %s", time.Since(begin))
	if n := owned(); n != 0 {
		t.Fatalf("%d VMs left after shutdown", n)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.removed) < 2 {
		t.Fatalf("idle runners not deregistered on shutdown: %v", gh.removed)
	}
}
