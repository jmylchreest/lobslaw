//go:build !linux

package sandbox

import "fmt"

// InstallAndExec is unsupported on non-Linux platforms. The reexec
// helper only runs on Linux where the kernel provides the relevant
// LSMs (Landlock) and BPF filter support (seccomp).
func InstallAndExec(p *Policy, path string, argv, env []string) error {
	_, _, _, _ = p, path, argv, env
	return fmt.Errorf("sandbox.InstallAndExec: %w", ErrUnsupportedPlatform)
}

// IsNoNewPrivsSet is unsupported off-Linux (no /proc/self/status).
func IsNoNewPrivsSet() (bool, error) {
	return false, ErrUnsupportedPlatform
}
