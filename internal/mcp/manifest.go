package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestFile is the conventional filename declaring MCP servers
// for a plugin or config bundle. Mirrors the Claude Code filename
// so operators can reuse existing manifests verbatim.
const ManifestFile = ".mcp.json"

// Manifest is the on-disk shape of a .mcp.json file. Servers is a
// map from an operator-chosen server name to its launch config.
type Manifest struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig is one server entry. Command + Args compose the
// argv the client spawns; Env is plain-text pairs; SecretEnv names
// env vars whose values come from secret refs (env:/file:/kms:)
// resolved before the subprocess spawns — same convention rclone
// and Telegram use. Everything is a concrete string in argv/env by
// the time the subprocess starts.
type ServerConfig struct {
	Command   string            `json:"command"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	SecretEnv map[string]string `json:"secret_env,omitempty"`

	// Disabled lets operators ship a .mcp.json with entries they've
	// temporarily turned off without removing the config. Skipped by
	// the loader.
	Disabled bool `json:"disabled,omitempty"`

	// Install runs once before the server spawns. Used to materialise
	// the server's binary into the cache (e.g. `uv tool install
	// minimax-mcp==1.27.0`). Idempotent — uvx/bunx no-op when the
	// version is already cached, so this is cheap on subsequent
	// boots. Pinning the version here is the supply-chain boundary;
	// loose pins ("latest") bypass the protection.
	Install []string `json:"install,omitempty"`
}

// LoadManifest reads + parses a .mcp.json file. Returns the parsed
// Manifest; errors surface on I/O failure or malformed JSON.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mcp: read %q: %w", path, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("mcp: parse %q: %w", path, err)
	}
	for name, cfg := range m.MCPServers {
		if name == "" {
			return nil, fmt.Errorf("mcp: %q: server entry with empty name", path)
		}
		if strings.ContainsAny(name, "/\\") {
			return nil, fmt.Errorf("mcp: %q: server name %q must not contain path separators", path, name)
		}
		if cfg.Command == "" && !cfg.Disabled {
			return nil, fmt.Errorf("mcp: %q: server %q has empty command", path, name)
		}
	}
	return &m, nil
}

// DiscoverManifests walks pluginsRoot for .mcp.json files one level
// deep — each plugin directory gets a single .mcp.json. Returns the
// parsed manifests paired with the directory they came from, sorted
// by directory so the resulting tool-name collisions are resolved
// deterministically across replicas.
func DiscoverManifests(pluginsRoot string) ([]DiscoveredManifest, error) {
	entries, err := os.ReadDir(pluginsRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("mcp: scan %q: %w", pluginsRoot, err)
	}
	var out []DiscoveredManifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		manifestPath := filepath.Join(pluginsRoot, e.Name(), ManifestFile)
		if _, err := os.Stat(manifestPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		m, err := LoadManifest(manifestPath)
		if err != nil {
			return nil, err
		}
		out = append(out, DiscoveredManifest{
			PluginDir: filepath.Join(pluginsRoot, e.Name()),
			Manifest:  m,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PluginDir < out[j].PluginDir })
	return out, nil
}

// DiscoveredManifest pairs a parsed .mcp.json with the plugin dir
// it lived in. PluginDir lets the manifest's SecretEnv references
// resolve relative paths against the correct plugin.
type DiscoveredManifest struct {
	PluginDir string
	Manifest  *Manifest
}

// ResolvedEnv walks cfg.SecretEnv + cfg.Env through the given
// resolver and returns a flat env slice ("KEY=value") ready for
// exec.Cmd.Env. Plain env takes precedence over secret_env on
// collision — an operator who lists the same key in both almost
// certainly means the literal value.
func (cfg ServerConfig) ResolvedEnv(resolve SecretResolver) ([]string, error) {
	merged := make(map[string]string, len(cfg.Env)+len(cfg.SecretEnv))
	for k, ref := range cfg.SecretEnv {
		if resolve == nil {
			return nil, errors.New("mcp: secret_env entries require a SecretResolver")
		}
		val, err := resolve(ref)
		if err != nil {
			return nil, fmt.Errorf("mcp: resolve secret %q: %w", k, err)
		}
		merged[k] = val
	}
	// plain env wins on collision with secret_env entries
	maps.Copy(merged, cfg.Env)
	out := make([]string, 0, len(merged))
	for k, v := range merged {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out, nil
}

// SecretResolver translates a ref string into its concrete secret
// value. Injected so pkg/config.ResolveSecret (or any scheme) can
// front the MCP loader without this package depending on it.
type SecretResolver func(ref string) (string, error)
