//go:build linux

package sandbox

import (
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// CapabilityReport describes what sandbox enforcement mechanisms
// are available on this kernel / build / runtime. Exposed via the
// debug_sandbox tool so operators can confirm the stack is
// genuinely active rather than silently no-oping.
type CapabilityReport struct {
	OS                  string `json:"os"`
	KernelVersion       string `json:"kernel_version,omitempty"`
	LandlockSupported   bool   `json:"landlock_supported"`
	LandlockABIVersion  int    `json:"landlock_abi_version,omitempty"`
	SeccompSupported    bool   `json:"seccomp_supported"`
	NoNewPrivsSupported bool   `json:"no_new_privs_supported"`
	CgroupV2Mounted     bool   `json:"cgroup_v2_mounted"`
	DaemonUnderSandbox  bool   `json:"daemon_under_sandbox"`
	SandboxMode         string `json:"sandbox_mode"` // "enforces-tools" | "none"
}

// Probe reports the live capabilities of the host. Safe to call
// repeatedly — no side effects, no process state changes. Works
// only on Linux; apply_other.go supplies a stub for other OSes.
func Probe() CapabilityReport {
	r := CapabilityReport{
		OS:          "linux",
		SandboxMode: "enforces-tools",
	}
	if v, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		r.KernelVersion = strings.TrimSpace(string(v))
	}

	// Landlock probe: prctl(PR_GET_SPECULATION_CTRL) is cheap and
	// always-present; landlock uses its own syscall. go-landlock's
	// ABIVersion isn't exposed as a pure-lookup function, but we
	// can check for the presence of the landlock interface files.
	if _, err := os.Stat("/sys/kernel/security/landlock"); err == nil {
		r.LandlockSupported = true
	} else if abi := readLandlockABI(); abi > 0 {
		r.LandlockSupported = true
		r.LandlockABIVersion = abi
	}

	// Seccomp probe: SECCOMP_GET_ACTION_AVAIL via seccomp(2) would
	// be ideal, but any SECCOMP_SET_MODE_FILTER prctl being
	// supported at all is sufficient. /proc/self/status carries a
	// Seccomp line: 0=disabled, 2=filtered.
	if raw, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "Seccomp:") {
				r.SeccompSupported = true
				if strings.Contains(line, "\t2") {
					r.DaemonUnderSandbox = true
				}
				break
			}
		}
	}

	// PR_SET_NO_NEW_PRIVS is universally supported on any kernel
	// that can run Go programs, but prove it with an actual prctl.
	if err := unix.Prctl(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0); err == nil {
		r.NoNewPrivsSupported = true
	}

	// cgroup v2 presence check — /sys/fs/cgroup/cgroup.controllers
	// exists on v2 systems; absent on legacy v1-only hosts.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		r.CgroupV2Mounted = true
	}

	return r
}

// readLandlockABI attempts to determine the kernel's landlock ABI
// version. Falls back to 0 when landlock isn't compiled in or the
// kernel is too old. A non-zero value implies Landlock is real and
// usable for filesystem restriction.
func readLandlockABI() int {
	raw, err := os.ReadFile("/sys/kernel/security/landlock/abi_version")
	if err != nil {
		return 0
	}
	var v int
	for _, b := range strings.TrimSpace(string(raw)) {
		if b < '0' || b > '9' {
			break
		}
		v = v*10 + int(b-'0')
	}
	return v
}
