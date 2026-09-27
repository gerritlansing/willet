package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestIsSessionConflict(t *testing.T) {
	conflicts := []string{
		// scaleset v0.4.0 formatting of a 409
		`failed to do the session request: request POST https://pipelines.actions.githubusercontent.com/x/_apis/runtime/runnerscalesets/1/sessions failed(status="409 Conflict", activity_id="a"): RunnerScaleSetSessionConflictException: The runner scale set msb already has an active session for owner host-a.`,
		// wording reported by actions-runner-controller users
		`failed to create session: 409 had issue communicating with Actions backend: The runner scale set gha-rs already has an active session for owner gha-rs-listener`,
	}
	for _, msg := range conflicts {
		if !isSessionConflict(errors.New(msg)) {
			t.Errorf("not detected: %s", msg)
		}
	}
	for _, msg := range []string{
		`request POST https://api.github.com/x failed(status="401 Unauthorized"): Bad credentials`,
		`request POST https://x/sessions failed(status="500 Internal Server Error")`,
		`dial tcp: connection refused`,
	} {
		if isSessionConflict(errors.New(msg)) {
			t.Errorf("false positive: %s", msg)
		}
	}
	if isSessionConflict(nil) {
		t.Error("nil is not a conflict")
	}
}

var errConflict = errors.New(`failed(status="409 Conflict"): already has an active session`)

func TestOpenSessionRetriesConflicts(t *testing.T) {
	calls := 0
	got, err := openSession(context.Background(), slog.New(slog.DiscardHandler), time.Millisecond, 4*time.Millisecond, func() (string, error) {
		calls++
		if calls < 4 {
			return "", errConflict
		}
		return "session", nil
	})
	if err != nil || got != "session" || calls != 4 {
		t.Fatalf("got %q, %v after %d calls", got, err, calls)
	}
}

func TestOpenSessionFailsFastOnOtherErrors(t *testing.T) {
	calls := 0
	_, err := openSession(context.Background(), slog.New(slog.DiscardHandler), time.Hour, time.Hour, func() (string, error) {
		calls++
		return "", errors.New(`failed(status="401 Unauthorized")`)
	})
	if err == nil || calls != 1 {
		t.Fatalf("want an immediate failure, got %v after %d calls", err, calls)
	}
}

func TestOpenSessionStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	begin := time.Now()
	_, err := openSession(ctx, slog.New(slog.DiscardHandler), time.Hour, time.Hour, func() (string, error) {
		return "", errConflict
	})
	if err == nil || time.Since(begin) > time.Second {
		t.Fatalf("want the conflict error promptly after cancel, got %v after %s", err, time.Since(begin))
	}
}
