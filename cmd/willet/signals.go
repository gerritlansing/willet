package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// handleSignals turns stop requests into two contexts. The first signal on
// sigs cancels drain, the second cancels force. Canceling parent, or calling
// stop, cancels both. force outlives drain, so work that must survive the
// first request can run under force.
func handleSignals(parent context.Context, sigs <-chan os.Signal) (drain, force context.Context, stop func()) {
	force, cancelForce := context.WithCancel(parent)
	drain, cancelDrain := context.WithCancel(force)
	go func() {
		select {
		case <-sigs:
			cancelDrain()
		case <-force.Done():
			return
		}
		select {
		case <-sigs:
			cancelForce()
		case <-force.Done():
		}
	}()
	return drain, force, cancelForce
}

// interruptContext is canceled by SIGINT or SIGTERM.
func interruptContext(parent context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
}
