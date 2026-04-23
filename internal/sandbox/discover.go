package sandbox

import (
	"os"
	"path/filepath"
)

// DiscoverPolicyDirs resolves the list of policy directories to
// load, in load order (later wins — matches last-write-wins
// precedence when the loader applies policies to the Registry).
//
// If explicit is non-empty, the caller specified directories
// explicitly (via config.toml's policy_dirs = [], env var, or
// --policy-dir flag) and we return those verbatim — no default
// discovery. This matches standard CLI ergonomics: "if I set
// --policy-dir, don't also use the implicit defaults."
//
// Otherwise, the default list is built in precedence order
// (earliest = lowest priority, analogous to git's system/global/
// local layering):
//
//  1. ~/.config/lobslaw/policy.d    — user-global
//  2. <configDir>/policy.d          — where resolved config.toml lives
//  3. <cwd>/policy.d                — workspace-local
//
// Entries resolving to the same canonical path (via EvalSymlinks)
// are de-duplicated; the first occurrence wins so the list order
// stays stable when, say, configDir==cwd (common in dev).
//
// Missing directories are kept in the list — LoadPolicyDirs treats
// each missing dir as a no-op so callers don't have to stat-check
// before plumbing the list through.
func DiscoverPolicyDirs(explicit []string, configDir string) []string {
	if len(explicit) > 0 {
		return dedupByRealpath(explicit)
	}

	candidates := make([]string, 0, 3)
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "lobslaw", "policy.d"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "lobslaw", "policy.d"))
	}
	if configDir != "" {
		candidates = append(candidates, filepath.Join(configDir, "policy.d"))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(cwd, "policy.d"))
	} else {
		candidates = append(candidates, "./policy.d")
	}

	return dedupByRealpath(candidates)
}

// dedupByRealpath removes duplicates while preserving order. Entries
// that successfully canonicalise (path exists, symlinks resolve) are
// compared by realpath; entries that can't be canonicalised are
// compared by their literal string. First occurrence wins.
func dedupByRealpath(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		key := p
		if real, err := filepath.EvalSymlinks(p); err == nil {
			key = real
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

