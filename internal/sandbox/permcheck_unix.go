//go:build unix

package sandbox

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// checkPolicyFilePerms verifies that a policy file is owned by the
// trusted UID and not group/world-writable. Returns a descriptive
// error when the file fails — caller logs + skips load.
//
// The UID check defeats the "attacker with write access under their
// own UID plants a policy" case. The mode check catches accidentally-
// too-permissive files from dev workflows or misconfigured volume
// mounts. Both together are the well-trodden pattern OpenSSH uses
// for ~/.ssh/config and sudo uses for /etc/sudoers.
//
// trustedUID < 0 means "don't check UID ownership". The mode check
// still runs because it's always worth enforcing.
func checkPolicyFilePerms(path string, info fs.FileInfo, trustedUID int, rejectWritableMask fs.FileMode) error {
	if rejectWritableMask != 0 && info.Mode().Perm()&rejectWritableMask != 0 {
		return fmt.Errorf("mode %o is too permissive (group/world-writable); rejecting", info.Mode().Perm())
	}
	if trustedUID < 0 {
		return nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read file ownership (platform: not a *syscall.Stat_t)")
	}
	if int(stat.Uid) != trustedUID {
		return fmt.Errorf("owned by uid %d, expected %d", stat.Uid, trustedUID)
	}
	return nil
}

// defaultTrustedUID returns the effective UID of the running agent,
// which is the default "who is allowed to author policy files" on
// Unix. Operators override via LoadOptions.TrustedUID when a
// different principal should be trusted (e.g. a dedicated
// lobslaw-admin UID).
func defaultTrustedUID() int { return os.Geteuid() }
