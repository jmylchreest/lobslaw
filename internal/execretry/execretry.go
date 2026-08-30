// Package execretry runs an exec.Cmd with a bounded retry on the
// errnos that mean "this binary is not ready to exec yet".
//
// It exists because the same forty lines lived in two packages —
// internal/compute's executor and internal/hooks' dispatcher — with
// identical constants, identical backoff and identical errno
// matching. The second copy's comment said so: "Same motivation as
// the compute executor's equivalent." Two copies of a retry loop with
// the same magic numbers drift, and the drift is silent: one gets a
// fix and the other keeps the bug.
//
// A leaf package with no lobslaw imports, so both callers can reach
// it. compute imports hooks, so this could not live in either.
package execretry

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	// maxAttempts and initialBackoff bound the retry. ETXTBSY
	// resolves within microseconds once the last writer's FD is
	// released, so this is short on purpose — it is a race window,
	// not a flaky network.
	maxAttempts    = 5
	initialBackoff = 10 * time.Millisecond
)

// Run executes cmd, retrying briefly while the binary reports as busy.
//
// ETXTBSY (Linux errno 26, "text file busy") surfaces when execve
// races the kernel's inode write-lock for a file that was just
// written and closed. tmpfs under parallel test load is the common
// trigger, but the same race appears in production during a skill,
// hook or tool binary replacement — plugin install, rolling update.
//
// EAGAIN is included because clone()'s pthread-spawn path reports the
// same transient as a different errno.
//
// Anything else returns immediately. Masking "binary not found" or a
// real ExitError behind five retries would turn a clear failure into
// a slow one.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	var err error
	backoff := initialBackoff
	for attempt := range maxAttempts {
		if attempt > 0 {
			// Checked before sleeping, not after: a cancelled context
			// means nobody is waiting for this, and the honest answer
			// is the error we already have.
			if ctx.Err() != nil {
				return err
			}
			cmd = cloneForRetry(ctx, cmd)
			time.Sleep(backoff)
			backoff *= 2
		}
		err = cmd.Run()
		if !IsTransient(err) {
			return err
		}
	}
	return err
}

// IsTransient reports whether err means "not ready yet" rather than
// "will not work".
//
// Exported because a caller that does its own running still wants the
// same answer, and a second copy of this predicate is how the two
// implementations diverged in the first place.
//
// errors.Is walks the exec.Error / os.PathError wrappers. The
// string-match fallback catches kernel and Go runtime combinations
// where the chain does not preserve the syscall errno cleanly — ugly,
// and load-bearing: without it the retry silently never fires on
// those platforms.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ETXTBSY) || errors.Is(err, syscall.EAGAIN) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "text file busy") ||
		strings.Contains(s, "resource temporarily unavailable")
}

// cloneForRetry builds a fresh exec.Cmd for a second Run.
//
// exec.Cmd is single-use — a second Run panics. It must be rebuilt
// via exec.CommandContext rather than by struct literal: the stdlib
// refuses to Run a Cmd that has a non-nil Cancel but was not built
// that way, which guards against a lost context cancellation.
//
// stdout/stderr writers are reused, so retry output is continuous.
// Safe because ETXTBSY means the first attempt never reached the
// program and produced nothing.
func cloneForRetry(ctx context.Context, src *exec.Cmd) *exec.Cmd {
	args := src.Args
	if len(args) == 0 {
		args = []string{src.Path}
	}
	fresh := exec.CommandContext(ctx, args[0], args[1:]...)
	fresh.Path = src.Path
	fresh.Env = src.Env
	fresh.Dir = src.Dir
	fresh.Stdin = src.Stdin
	fresh.Stdout = src.Stdout
	fresh.Stderr = src.Stderr
	fresh.ExtraFiles = src.ExtraFiles
	fresh.SysProcAttr = src.SysProcAttr
	fresh.WaitDelay = src.WaitDelay
	return fresh
}
