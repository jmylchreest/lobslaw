package binaries

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
)

// Manager installs a binary using one specific package-manager-style
// channel. The Registry maps Manager.Name() → implementation.
type Manager interface {
	// Name returns the canonical name ("apt", "brew", ...).
	Name() string

	// Hosts returns the upstream hostnames this manager will hit.
	// Used to seed the smokescreen "binaries-install" egress role.
	// May depend on the spec (e.g. curl-sh's host is the URL host).
	Hosts(spec InstallSpec) []string

	// Available reports whether this manager can run on the current
	// host (binary present in PATH; OS matches; etc.).
	Available(ctx context.Context) bool

	// Install runs the install. Returns nil on success. The runner
	// is responsible for writing stdout/stderr to slog and for
	// honouring ctx cancellation.
	Install(ctx context.Context, spec InstallSpec, runner ProcessRunner, log *slog.Logger) error
}

// ProcessRunner is the seam tests stub. Production wiring uses the
// shellRunner below; tests replace with a recording runner.
type ProcessRunner interface {
	// Run invokes the named command and returns combined stdout +
	// stderr plus the exit code. Returns 0/nil only on actual
	// successful exec; non-zero exit is returned with the captured
	// output and a *exec.ExitError.
	Run(ctx context.Context, name string, args []string, env []string) (output string, err error)
}

// ShellRunner is the production ProcessRunner.
type ShellRunner struct{}

// Run executes via os/exec.
func (ShellRunner) Run(ctx context.Context, name string, args []string, env []string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// errSudoNotAllowed is returned when a spec requires sudo but the
// process can't elevate (running as non-root outside a passwordless
// sudo arrangement). Surfaces to the agent so the operator can fix.
var errSudoNotAllowed = errors.New("install requires sudo but lobslaw is not root and passwordless sudo is not configured")

// errManagerNotAvailable is returned when the manager binary isn't
// in PATH or the OS doesn't support it.
var errManagerNotAvailable = errors.New("manager binary not available on this host")

// runManagerCmd is the typical "shell out to the manager" wrapper
// shared by apt/brew/pacman/dnf/etc. It honours sudo, passes args,
// and returns a tagged error on failure.
func runManagerCmd(ctx context.Context, runner ProcessRunner, log *slog.Logger,
	managerName string, sudo bool, args []string,
) error {
	bin := managerName
	finalArgs := args
	if sudo {
		if err := ensureSudoAllowed(ctx, runner); err != nil {
			return err
		}
		bin = "sudo"
		finalArgs = append([]string{"-n", managerName}, args...)
	}
	log.Info("binaries: install", "manager", managerName, "args", args, "sudo", sudo)
	out, err := runner.Run(ctx, bin, finalArgs, nil)
	if err != nil {
		log.Error("binaries: install failed",
			"manager", managerName, "err", err, "output", out)
		return fmt.Errorf("%s install: %w (output: %s)", managerName, err, truncate(out, 512))
	}
	log.Info("binaries: install ok", "manager", managerName, "output", truncate(out, 256))
	return nil
}

// ensureSudoAllowed runs `sudo -n true` to verify passwordless sudo
// works without prompting. Fails fast with errSudoNotAllowed when
// it doesn't.
func ensureSudoAllowed(ctx context.Context, runner ProcessRunner) error {
	out, err := runner.Run(ctx, "sudo", []string{"-n", "true"}, nil)
	if err != nil {
		return fmt.Errorf("%w: sudo -n probe: %v (output: %s)", errSudoNotAllowed, err, truncate(out, 256))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
