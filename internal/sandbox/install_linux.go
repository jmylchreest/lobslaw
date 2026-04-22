//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"slices"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
	"golang.org/x/sys/unix"
)

// InstallAndExec is the child-side of the reexec sandbox helper. It
// runs inside the spawned subprocess (post-fork, after namespace
// setup done by Apply's Cloneflags) and performs the enforcement
// installs that Go's stdlib doesn't expose via SysProcAttr:
//
//  1. PR_SET_NO_NEW_PRIVS (prereq for Landlock; blocks setuid)
//  2. Landlock filesystem restriction (see Phase 4.5.5b)
//  3. Seccomp BPF syscall filter (see Phase 4.5.5c)
//
// Then it calls execve(path, argv, env). On success this function
// does not return — the target binary takes over the process.
//
// Order is load-bearing: Landlock install requires NoNewPrivs; seccomp
// is installed last so a future tightening of its deny-list can never
// accidentally block Landlock's own syscalls.
//
// The caller (cmd/lobslaw sandbox-exec subcommand) is responsible for
// clearing any transport env vars (e.g. LOBSLAW_SANDBOX_POLICY)
// before calling this, so the target binary inherits a clean env.
func InstallAndExec(p *Policy, path string, argv, env []string) error {
	if p == nil {
		return fmt.Errorf("InstallAndExec: nil Policy")
	}
	if path == "" {
		return fmt.Errorf("InstallAndExec: empty target path")
	}
	if len(argv) == 0 {
		return fmt.Errorf("InstallAndExec: empty argv (argv[0] is required)")
	}

	if p.NoNewPrivs {
		if err := setNoNewPrivs(); err != nil {
			return fmt.Errorf("set PR_SET_NO_NEW_PRIVS: %w", err)
		}
	}

	// Landlock install (Phase 4.5.5b).
	if err := installLandlock(p); err != nil {
		return fmt.Errorf("install landlock: %w", err)
	}

	// Seccomp install (Phase 4.5.5c).
	if err := installSeccomp(p); err != nil {
		return fmt.Errorf("install seccomp: %w", err)
	}

	return syscall.Exec(path, argv, env)
}

// setNoNewPrivs sets PR_SET_NO_NEW_PRIVS=1 on the current process.
// Once set, any execve (including the one at the end of
// InstallAndExec) cannot gain capabilities or setuid bits — the
// critical prerequisite for Landlock enforcement.
func setNoNewPrivs() error {
	// prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0).
	return unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)
}

// installLandlock restricts filesystem access via the Landlock LSM
// when the policy declares AllowedPaths. The permitted access set is:
//
//   - All AllowedPaths directories: read + write + create + delete
//   - All ReadOnlyPaths directories: read only (subset of AllowedPaths)
//
// Anything outside these paths returns EACCES on syscall entry,
// kernel-enforced. Uses .BestEffort() so older kernels (5.13+) still
// benefit from partial enforcement; a kernel without Landlock at all
// silently no-ops.
//
// Landlock requires PR_SET_NO_NEW_PRIVS=1 — the library sets it for
// us if we haven't already (though we typically have via Policy.NoNewPrivs).
//
// No-op when AllowedPaths is empty; Normalise already populated any
// sensible defaults.
func installLandlock(p *Policy) error {
	if len(p.AllowedPaths) == 0 {
		return nil
	}

	// Partition AllowedPaths into RW vs RO sets. Entries in
	// ReadOnlyPaths get the read-only rule; everything else is RW.
	var roDirs, rwDirs []string
	for _, path := range p.AllowedPaths {
		if slices.Contains(p.ReadOnlyPaths, path) {
			roDirs = append(roDirs, path)
		} else {
			rwDirs = append(rwDirs, path)
		}
	}

	rules := make([]landlock.Rule, 0, 2)
	if len(roDirs) > 0 {
		rules = append(rules, landlock.RODirs(roDirs...).IgnoreIfMissing())
	}
	if len(rwDirs) > 0 {
		rules = append(rules, landlock.RWDirs(rwDirs...).IgnoreIfMissing())
	}

	return landlock.V5.BestEffort().RestrictPaths(rules...)
}

// installSeccomp is stubbed out until Phase 4.5.5c wires the
// elastic/go-seccomp-bpf library. No-op until then.
func installSeccomp(p *Policy) error {
	_ = p
	return nil
}

// IsNoNewPrivsSet reads /proc/self/status and reports whether the
// calling process has PR_SET_NO_NEW_PRIVS=1. Used by tests.
func IsNoNewPrivsSet() (bool, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false, err
	}
	for line := range splitLines(string(data)) {
		if len(line) > 12 && line[:12] == "NoNewPrivs:\t" {
			return line[12:] == "1", nil
		}
	}
	return false, fmt.Errorf("NoNewPrivs line not found in /proc/self/status")
}

// splitLines yields each line (without trailing '\n') from s.
func splitLines(s string) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for len(s) > 0 {
			i := 0
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if !yield(s[:i]) {
				return
			}
			if i < len(s) {
				s = s[i+1:]
			} else {
				s = ""
			}
		}
	}
}
