//go:build !linux

package sandbox

import (
	"errors"
	"os/exec"
)

// ErrUnsupportedPlatform is returned by Apply on non-Linux systems.
// Namespace-based sandboxing is a Linux-specific feature; operators
// running on macOS or Windows run without the sandbox (and should
// be aware of that choice).
var ErrUnsupportedPlatform = errors.New("sandbox namespace support is Linux-only")

// Apply is a no-op on non-Linux platforms. Returns
// ErrUnsupportedPlatform when a sandbox was actually configured so
// callers don't silently run without the protections they asked for.
func Apply(cmd *exec.Cmd, p *Policy) error {
	if p == nil {
		return nil
	}
	if p.Namespaces.Enabled() || p.NoNewPrivs || p.Seccomp.HasRules() || p.CPUQuota > 0 || p.MemoryLimitMB > 0 {
		return ErrUnsupportedPlatform
	}
	return nil
}
