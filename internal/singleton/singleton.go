// Package singleton coordinates "exactly-one-owner" workloads
// across cluster nodes. The current Gate implementation pins
// ownership to whichever node holds raft leadership; a future
// per-name lease implementation can replace it without changing the
// caller-facing API.
package singleton

import (
	"context"
	"errors"
	"log/slog"
)

// Gate decides when this node currently owns the named singleton.
// Owned and Watch must be safe for concurrent use.
type Gate interface {
	// Owned reports whether this node currently owns name. Cheap.
	Owned(name string) bool
	// Watch returns a channel of ownership transitions for name.
	// The channel is buffered with the latest state so a caller that
	// subscribes after a transition still sees the current value on
	// its first receive. Stop the subscription with Unwatch.
	Watch(name string) <-chan bool
	// Unwatch releases the resources tied to a Watch subscription.
	// Safe to call with a channel that was never Watch'd.
	Unwatch(name string, ch <-chan bool)
}

// Run drives fn for the lifetime of ownership: invoked with a fresh
// child context whenever this node gains ownership of name, the
// child context cancelled when ownership is lost. Returns when the
// parent ctx is cancelled or fn returns a non-nil error that is not
// context.Canceled.
func Run(ctx context.Context, gate Gate, name string, log *slog.Logger, fn func(ctx context.Context) error) error {
	if log == nil {
		log = slog.Default()
	}
	ch := gate.Watch(name)
	defer gate.Unwatch(name, ch)

	var (
		runCtx    context.Context
		runCancel context.CancelFunc
		runDone   chan error
	)
	stopRun := func() {
		if runCancel != nil {
			runCancel()
			<-runDone
			runCancel = nil
			runDone = nil
		}
	}
	defer stopRun()

	startRun := func() {
		runCtx, runCancel = context.WithCancel(ctx)
		runDone = make(chan error, 1)
		go func() {
			runDone <- fn(runCtx)
		}()
	}

	// Seed initial state from the buffered channel.
	owned := gate.Owned(name)
	if owned {
		log.Info("singleton acquired", "name", name)
		startRun()
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case next, ok := <-ch:
			if !ok {
				return nil
			}
			if next == owned {
				continue
			}
			owned = next
			if owned {
				log.Info("singleton acquired", "name", name)
				startRun()
			} else {
				log.Info("singleton released", "name", name)
				stopRun()
			}

		case err := <-runDone:
			runCancel = nil
			runDone = nil
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("singleton handler returned error", "name", name, "err", err)
				return err
			}
			// fn returned cleanly while we still believe we own it —
			// nothing more to do; wait for ownership change or ctx.
		}
	}
}
