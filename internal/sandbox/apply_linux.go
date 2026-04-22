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
// clone flags, UID/GID mappings for the user namespace, and (when
// enforcement is requested) rewrites cmd to reexec through the
// sandbox helper subcommand.
//
// What's enforced today:
//
//   - CLONE_NEWUSER/NEWNS/NEWPID/NEWNET/NEWUTS/NEWIPC based on p.Namespaces
//   - UidMappings/GidMappings (map the calling UID to root-inside-userns,
//     the standard unprivileged pattern)
//   - PR_SET_NO_NEW_PRIVS + Landlock + seccomp via the reexec helper
//     — cmd is rewritten to invoke /proc/self/exe sandbox-exec, which
//     installs these layers in the child post-fork pre-execve.
//
// Deferred (see DEFERRED.md):
//
//   - pivot_root (superseded by Landlock)
//   - cgroup v2 cpu.max and memory.max writes
//   - nftables egress rules in the network namespace
//
// Apply is a no-op when p is nil or all fields are zero.
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

	// Rewrite cmd to reexec through /proc/self/exe sandbox-exec when
	// the policy asks for NoNewPrivs / Landlock / seccomp enforcement.
	// The helper runs inside the namespaces set up above, then
	// performs the three installs before execve'ing the real target.
	if needsReexec(p) {
		if err := rewriteForHelperReexec(cmd, p); err != nil {
			return fmt.Errorf("rewrite for sandbox helper: %w", err)
		}
	}

	return nil
}

// needsReexec reports whether the Policy has enforcement fields set
// that the reexec helper is responsible for — NoNewPrivs, Landlock
// (AllowedPaths), or Seccomp. Namespaces alone don't require the
// helper (they're applied via SysProcAttr.Cloneflags by Apply).
func needsReexec(p *Policy) bool {
	if p == nil {
		return false
	}
	return p.NoNewPrivs || len(p.AllowedPaths) > 0 || p.Seccomp.HasRules()
}

// rewriteForHelperReexec mutates cmd so it invokes the running
// binary's `sandbox-exec` subcommand rather than the original target
// directly. The helper reads the Policy from env, installs kernel
// enforcement, then execve's the original target so the caller sees
// the expected argv.
//
// Expects cmd.Args[0] to match cmd.Path — the Executor builds commands
// this way (via exec.CommandContext which sets Args[0] = name).
func rewriteForHelperReexec(cmd *exec.Cmd, p *Policy) error {
	encoded, err := EncodePolicy(p)
	if err != nil {
		return fmt.Errorf("encode policy: %w", err)
	}
	originalPath := cmd.Path
	originalArgs := cmd.Args
	if len(originalArgs) == 0 {
		// Shouldn't happen — exec.Command always populates Args — but
		// defend against a caller that manually cleared the slice.
		originalArgs = []string{originalPath}
	}

	// /proc/self/exe always resolves to the current binary inode, so
	// the helper runs with whatever build the parent was invoked from —
	// no need to know the filesystem path to the lobslaw binary.
	cmd.Path = "/proc/self/exe"
	cmd.Args = append(
		[]string{"lobslaw", HelperSubcommand, "--", originalPath},
		originalArgs[1:]...,
	)

	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Env = append(cmd.Env, PolicyEnvVar+"="+encoded)
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
