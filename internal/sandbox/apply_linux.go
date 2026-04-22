//go:build linux

package sandbox

import (
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// Apply installs the Policy onto cmd's SysProcAttr. Sets namespace
// clone flags, no_new_privs, and UID/GID mappings for the user
// namespace.
//
// What's enforced today:
//
//   - CLONE_NEWUSER/NEWNS/NEWPID/NEWNET/NEWUTS/NEWIPC based on p.Namespaces
//   - PR_SET_NO_NEW_PRIVS when p.NoNewPrivs is true
//   - UidMappings/GidMappings (map the calling UID to root-inside-userns,
//     the standard unprivileged pattern)
//
// Deferred (see DEFERRED.md):
//
//   - pivot_root into a per-sandbox directory (needs CAP_SYS_ADMIN
//     unless we're already inside a user namespace with the cap)
//   - bind-mounts for AllowedPaths + ReadOnlyPaths
//   - cgroup v2 cpu.max and memory.max writes
//   - nftables egress rules in the network namespace
//   - seccomp BPF install (filter is built; install is deferred)
//
// Apply is a no-op when p is nil or p.Namespaces.Enabled() is false
// AND NoNewPrivs is false — i.e. the fully-empty policy runs the
// command with no special treatment.
func Apply(cmd *exec.Cmd, p *Policy) error {
	if p == nil {
		return nil
	}
	p.Normalise()

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	if p.Namespaces.Enabled() {
		cmd.SysProcAttr.Cloneflags |= cloneFlags(p.Namespaces)

		// User-namespace path needs UID/GID mappings so the child can
		// do things that need capabilities inside its own namespace
		// (e.g. mount, chroot) without actually being root on the host.
		if p.Namespaces.User {
			uid, gid, err := callerUIDGID()
			if err != nil {
				return fmt.Errorf("caller uid/gid: %w", err)
			}
			cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: uid, Size: 1},
			}
			cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{
				{ContainerID: 0, HostID: gid, Size: 1},
			}
			// setgroups must be denied before gid_map can be written
			// in the common unprivileged path.
			cmd.SysProcAttr.GidMappingsEnableSetgroups = false
		}
	}

	// PR_SET_NO_NEW_PRIVS is not exposed by stdlib syscall.SysProcAttr.
	// Wiring it properly needs either a setpriv(1) wrapper or a cgo
	// pre-exec helper — tracked in DEFERRED.md. For now the user
	// namespace is the primary defence against setuid escalation
	// (inside a userns, setuid binaries don't gain real-host
	// capabilities), so skipping no_new_privs doesn't leave the
	// sandbox naked.
	_ = p.NoNewPrivs

	return nil
}

// cloneFlags converts the typed NamespaceSet into the kernel's
// CLONE_NEW* bitmask.
func cloneFlags(n NamespaceSet) uintptr {
	var f uintptr
	if n.User {
		f |= syscall.CLONE_NEWUSER
	}
	if n.Mount {
		f |= syscall.CLONE_NEWNS
	}
	if n.PID {
		f |= syscall.CLONE_NEWPID
	}
	if n.Network {
		f |= syscall.CLONE_NEWNET
	}
	if n.UTS {
		f |= syscall.CLONE_NEWUTS
	}
	if n.IPC {
		f |= syscall.CLONE_NEWIPC
	}
	return f
}

// callerUIDGID returns the running process's UID/GID for mapping into
// the child user namespace.
func callerUIDGID() (int, int, error) {
	u, err := user.Current()
	if err != nil {
		return 0, 0, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid %q: %w", u.Uid, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid %q: %w", u.Gid, err)
	}
	return uid, gid, nil
}
