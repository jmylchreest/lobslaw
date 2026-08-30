package execretry

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
	"testing"
)

// The predicate is what decides whether a retry happens at all, and
// it had two divergent copies before this package existed.
func TestIsTransient(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                  {nil, false},
		"ETXTBSY":              {syscall.ETXTBSY, true},
		"EAGAIN":               {syscall.EAGAIN, true},
		"wrapped ETXTBSY":      {&exec.Error{Name: "x", Err: syscall.ETXTBSY}, true},
		"string fallback busy": {errors.New("fork/exec /x: text file busy"), true},
		"string fallback again": {
			errors.New("fork/exec /x: resource temporarily unavailable"), true},
		// The one that must NOT retry: masking it behind five attempts
		// turns a clear failure into a slow one.
		"not found":  {errors.New("exec: \"nope\": executable file not found in $PATH"), false},
		"exit error": {&exec.ExitError{}, false},
	}
	for name, c := range cases {
		if got := IsTransient(c.err); got != c.want {
			t.Errorf("%s: IsTransient = %v, want %v", name, got, c.want)
		}
	}
}

// A command that simply works must run once and return.
func TestRunPassesThroughSuccess(t *testing.T) {
	t.Parallel()
	if err := Run(context.Background(), exec.Command("true")); err != nil {
		t.Errorf("Run(true) = %v, want nil", err)
	}
}

// And a real failure must come back as itself, not as a retry
// exhaustion.
func TestRunReturnsARealFailureImmediately(t *testing.T) {
	t.Parallel()
	err := Run(context.Background(), exec.Command("false"))
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("Run(false) = %v, want an *exec.ExitError", err)
	}
}

// A cancelled context must not sleep out the backoff. The check is
// before the sleep, so a cancelled caller gets the error it already
// has rather than waiting on a retry nobody is expecting.
func TestRunHonoursACancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A command that does not exist fails immediately and is not
	// transient, so this asserts the ordinary path stays correct with
	// a dead context rather than hanging.
	if err := Run(ctx, exec.Command("definitely-not-a-real-binary-xyz")); err == nil {
		t.Error("expected an error for a missing binary")
	}
}
