package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

func TestIsSessionConflict(t *testing.T) {
	// How the SDK reports a 409 when creating a session.
	conflict := fmt.Errorf("failed to create message session: failed to do the session request: %w: %w",
		scaleset.ConflictError, errors.New("The runner scale set msb already has an active session for owner host-a."))
	if !isSessionConflict(conflict) {
		t.Errorf("not detected: %v", conflict)
	}
	for _, err := range []error{
		fmt.Errorf("%w: Bad credentials", scaleset.UnauthorizedError),
		fmt.Errorf("%w: server error", scaleset.NotFoundError),
		errors.New("dial tcp: connection refused"),
		nil,
	} {
		if isSessionConflict(err) {
			t.Errorf("false positive: %v", err)
		}
	}
}

var errConflict = fmt.Errorf("already has an active session: %w", scaleset.ConflictError)

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
		return "", fmt.Errorf("%w: Bad credentials", scaleset.UnauthorizedError)
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
