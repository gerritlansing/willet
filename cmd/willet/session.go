package main

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

// isSessionConflict reports whether err is GitHub refusing a message session
// because the scale set already has one. That happens when another host runs
// the same scale set, or for a while after a daemon crashed without closing
// its session. scaleset v0.4.0 has no typed error for this, so match the
// status line it formats and GitHub's message text.
func isSessionConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, `status="409 Conflict"`) ||
		strings.Contains(msg, "SessionConflict") ||
		strings.Contains(msg, "already has an active session")
}

// openSession calls open until it succeeds, fails with something other than a
// session conflict, or ctx ends. Conflicts are retried with backoff, since a
// stale session expires on GitHub's side.
func openSession[T any](ctx context.Context, logger *slog.Logger, backoff, maxBackoff time.Duration, open func() (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		s, err := open()
		if err == nil || !isSessionConflict(err) {
			return s, err
		}
		logger.Warn("GitHub reports the scale set already has an active session, retrying. "+
			"This clears by itself if a previous daemon crashed; if another host runs this scale set, stop one of them.",
			slog.Int("attempt", attempt), slog.Duration("retryIn", backoff))
		select {
		case <-ctx.Done():
			var zero T
			return zero, err
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}
