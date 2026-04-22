package sandbox

import (
	"os"
	"path/filepath"
)

// DiscoverPolicyDir resolves the conventional `policy.d/` directory
// alongside the agent's config. Discovery order:
//
//  1. If explicit is non-empty, use it verbatim.
//  2. If configDir is non-empty, return filepath.Join(configDir, "policy.d").
//  3. Fall back to "./policy.d" relative to CWD.
//
// Callers pass the resolved config file's directory (Config.Dir()
// from pkg/config) as configDir. When config.toml wasn't found (env-
// only mode), configDir is "" and we use the CWD fallback — typical
// in containers where the agent runs with $LOBSLAW_CONFIG unset and
// policies sit next to the binary.
//
// Always returns a path; whether it exists is not checked here —
// LoadPolicyDir treats a missing path as a no-op so the caller can
// unconditionally plumb the result through.
func DiscoverPolicyDir(explicit, configDir string) string {
	if explicit != "" {
		return explicit
	}
	if configDir != "" {
		return filepath.Join(configDir, "policy.d")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "./policy.d"
	}
	return filepath.Join(cwd, "policy.d")
}
