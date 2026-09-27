package main

import (
	"context"
	"errors"
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
	if err := run(ctx, cfg); !errors.Is(err, owner.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}

	// Control: with the lock free, run gets past it and fails later, on the
	// canceled context. This proves the check above isn't vacuous.
	held.Release()
	if err := run(ctx, cfg); err == nil || errors.Is(err, owner.ErrLocked) {
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
	if err := run(ctx, lockTestConfig()); errors.Is(err, owner.ErrLocked) {
		t.Fatalf("a different org's lock blocked this scale set: %v", err)
	}
}
