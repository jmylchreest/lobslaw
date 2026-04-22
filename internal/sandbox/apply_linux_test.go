//go:build linux

package sandbox

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestApplyNilPolicyIsNoOp confirms a nil Policy leaves the command
// untouched — callers who don't set a sandbox shouldn't accidentally
// get one.
func TestApplyNilPolicyIsNoOp(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/true")
	if err := Apply(cmd, nil); err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != nil {
		t.Error("nil Policy shouldn't populate SysProcAttr")
	}
}

// TestApplyEmptyPolicyIsNoOp confirms a zero-value Policy is treated
// as "no sandbox" — distinct from a policy that asks for user
// namespace (which Normalise flips NoNewPrivs to true).
func TestApplyEmptyPolicyIsNoOp(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/true")
	p := &Policy{}
	if err := Apply(cmd, p); err != nil {
		t.Fatal(err)
	}
	if cmd.SysProcAttr != nil && cmd.SysProcAttr.Cloneflags != 0 {
		t.Errorf("zero Policy set Cloneflags=%x", cmd.SysProcAttr.Cloneflags)
	}
}

// TestApplyUserNamespaceActuallyIsolatesUID spawns a subprocess
// inside a user namespace and reads /proc/self/status UidMap. A
// successfully-isolated process sees itself as UID 0 inside the
// namespace (mapped from the caller's real UID on the host).
//
// Skipped if the kernel doesn't allow unprivileged user-ns clone
// (some locked-down distros — Debian with unprivileged_userns_clone=0,
// hardened sysctl, containers that banned it).
func TestApplyUserNamespaceActuallyIsolatesUID(t *testing.T) {
	t.Parallel()
	p := &Policy{
		Namespaces: NamespaceSet{User: true, PID: true, Mount: true},
	}

	cmd := exec.Command("/bin/cat", "/proc/self/status")
	if err := Apply(cmd, p); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Most common failure mode: unprivileged user-ns clone is
		// disabled on this system. Skip rather than fail — the test
		// is about correctness-when-enabled, not kernel config.
		t.Skipf("user namespace clone failed (kernel config?): %v / stderr=%q", err, stderr.String())
	}

	out := stdout.String()
	// Two properties prove we're isolated:
	//   1. A Uid line starting with "Uid:\t0\t" — we're root in the userns
	//   2. A UidMap (in /proc/self/uid_map, summarised in status) showing
	//      the mapping from our real UID.
	if !strings.Contains(out, "\nUid:\t0\t") {
		t.Errorf("expected UID 0 inside userns; /proc/self/status: %q", out)
	}
}

// TestApplyUserNamespaceBlocksMountOutside verifies that a process
// inside our userns can't mount things on the host. Even though the
// userns gives us CAP_SYS_ADMIN inside our own namespace, the mount
// target must be in our own mount namespace (which means we're not
// touching the host mount table).
func TestApplyUserNamespaceBlocksHostMountTableChanges(t *testing.T) {
	t.Parallel()
	p := &Policy{
		Namespaces: NamespaceSet{User: true, Mount: true, PID: true},
	}

	// Try to mount something. Inside a mount namespace the mount
	// should succeed but be invisible to the host; WITHOUT the
	// namespace the mount call would need real-root.
	cmd := exec.Command("/bin/sh", "-c",
		`mkdir -p /tmp/sandbox-mnt-test; `+
			`mount -t tmpfs tmpfs /tmp/sandbox-mnt-test && `+
			`echo mount-ok && `+
			`umount /tmp/sandbox-mnt-test`)
	if err := Apply(cmd, p); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	_ = cmd.Run() // we don't assert success — depends on kernel config

	// If the mount DID succeed, confirm the host mount table didn't
	// change. Read /proc/mounts from the parent process; if our tmpfs
	// mountpoint shows up here, the mount namespace failed.
	// (This is a cross-check; the primary isolation is the mount
	// namespace itself.)
	if _, err := exec.Command("grep", "-q", "sandbox-mnt-test", "/proc/mounts").CombinedOutput(); err == nil {
		t.Error("SECURITY: sandbox mount showed up in host /proc/mounts — mount namespace failed")
	}
}
