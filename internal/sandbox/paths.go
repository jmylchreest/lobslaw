package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CanonicalizeAndContain resolves path via realpath (EvalSymlinks),
// normalises it, and verifies it sits under one of roots. Returns
// the canonical path on success. Rejects:
//
//   - non-absolute paths
//   - paths whose realpath contains ".." segments (belt-and-braces;
//     EvalSymlinks should normalise these out)
//   - paths resolving outside the configured roots
//
// Empty roots slice means "no containment check" — useful when the
// caller just wants a canonical form.
func CanonicalizeAndContain(path string, roots []string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("evalsymlinks %q: %w", path, err)
	}
	if strings.Contains(resolved, "/../") || strings.HasSuffix(resolved, "/..") {
		return "", fmt.Errorf("path %q contains traversal after resolution (%q)", path, resolved)
	}

	if len(roots) == 0 {
		return resolved, nil
	}

	for _, root := range roots {
		rootResolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootResolved, resolved)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && rel != "") {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q (resolved to %q) is outside allowed roots", path, resolved)
}

// RequireSingleLink returns an error if the inode at path has more
// than one hardlink. Use this as an opt-in hardening for paths where
// a multi-linked file would be a security concern — e.g. tool
// executables inside an AllowedPathRoot that a lesser-privileged
// user could have linked to /etc/shadow.
//
// Most operators will set this only on paths owned by untrusted UIDs.
// See DEFERRED.md "sandbox.hardlink-rejection" for the policy
// discussion.
func RequireSingleLink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("lstat %q: %w", path, err)
	}
	// Directories naturally have st_nlink > 1 (entries for . and ..).
	// This check applies to regular files and symlinks only.
	if info.IsDir() {
		return fmt.Errorf("%q is a directory; st_nlink check applies to files only", path)
	}
	nlink, ok := statNlink(info)
	if !ok {
		// Platform doesn't expose Nlink (Windows); skip rather than
		// fail — this is Linux-specific hardening.
		return nil
	}
	if nlink > 1 {
		return fmt.Errorf("%q has %d hardlinks; multi-linked paths are rejected", path, nlink)
	}
	return nil
}
