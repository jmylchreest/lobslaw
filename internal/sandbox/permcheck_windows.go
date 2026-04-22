//go:build windows

package sandbox

import (
	"io/fs"
	"log/slog"
	"sync"
)

// Windows doesn't have Unix mode bits or a cheap st_uid to inspect;
// meaningful file integrity checks would need to walk NTFS ACLs via
// golang.org/x/sys/windows — a much bigger lift. For now, policies
// load unconditionally on Windows and we log the gap once so
// operators aren't surprised.
//
// Threat-model note: sandbox enforcement itself is already no-op on
// Windows (see install_other.go), so the loader running without a
// permission check doesn't expose enforcement that wouldn't also be
// bypassed at runtime. The check exists for defence-in-depth on Linux
// where the kernel enforces the policy the loader approved.

var windowsPermWarnOnce sync.Once

func checkPolicyFilePerms(path string, info fs.FileInfo, trustedUID int, rejectWritableMask fs.FileMode) error {
	_, _, _, _ = path, info, trustedUID, rejectWritableMask
	windowsPermWarnOnce.Do(func() {
		slog.Warn("sandbox: policy file permission checks are not enforced on Windows — " +
			"protect the policy directory via filesystem ACLs")
	})
	return nil
}

// defaultTrustedUID returns -1 on Windows (meaning "skip UID check").
// The loader treats any negative trustedUID as "don't check" so the
// Unix and Windows code paths share the same LoadOptions semantics.
func defaultTrustedUID() int { return -1 }
