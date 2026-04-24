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

// CapabilityReport is a minimal non-Linux stub. Every capability
// reports false; debug_sandbox surfaces this so operators on
// non-Linux hosts see the clear "sandbox not available" shape
// rather than an empty object.
type CapabilityReport struct {
	OS                  string `json:"os"`
	KernelVersion       string `json:"kernel_version,omitempty"`
	LandlockSupported   bool   `json:"landlock_supported"`
	LandlockABIVersion  int    `json:"landlock_abi_version,omitempty"`
	SeccompSupported    bool   `json:"seccomp_supported"`
	NoNewPrivsSupported bool   `json:"no_new_privs_supported"`
	CgroupV2Mounted     bool   `json:"cgroup_v2_mounted"`
	DaemonUnderSandbox  bool   `json:"daemon_under_sandbox"`
	SandboxMode         string `json:"sandbox_mode"`
}

// Probe returns a zero report with SandboxMode="none" on non-Linux.
func Probe() CapabilityReport {
	return CapabilityReport{OS: "non-linux", SandboxMode: "none"}
}
