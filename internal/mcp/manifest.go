package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
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
// env vars whose values come from secret refs (env:, file:, or any
// [[secrets.providers]] label)
// resolved before the subprocess spawns — same convention rclone
// and Telegram use. Everything is a concrete string in argv/env by
// the time the subprocess starts.
type ServerConfig struct {
	// URL makes this a REMOTE server over MCP's HTTP+SSE binding
	// rather than a spawned subprocess. Mutually exclusive with
	// Command.
	URL string `json:"url,omitempty"`

	// BearerToken is a secret ref whose value becomes
	// "Authorization: Bearer <value>".
	//
	// Its own field because the secret is the TOKEN, not the header:
	// putting "Bearer " into the vault entry makes the stored secret
	// a header value, which is wrong the moment anything else wants
	// the same token. Mirrors StaticCredential's Header+Prefix pair
	// in internal/compute, where the scheme is the code's business
	// and the credential is the operator's.
	BearerToken string `json:"bearer_token,omitempty"`

	// Headers and SecretHeaders are for everything else. Values are
	// used verbatim, so an API wanting a raw key in Authorization —
	// some do — is expressible without fighting a prefix it never
	// asked for.
	Headers       map[string]string `json:"headers,omitempty"`
	SecretHeaders map[string]string `json:"secret_headers,omitempty"`

	// Networks is the host allowlist for a stdio server's own
	// outbound traffic. A remote server needs none: its URL host is
	// the allowlist.
	Networks []string `json:"networks,omitempty"`

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

// Remote reports whether this server is reached over HTTP rather
// than spawned.
func (cfg ServerConfig) Remote() bool { return strings.TrimSpace(cfg.URL) != "" }

// ResolvedHeaders merges plain and secret headers for a remote
// server, resolving refs the same way ResolvedEnv does.
//
// Plain wins on collision, matching ResolvedEnv: one rule for both,
// rather than two orders to remember.
func (cfg ServerConfig) ResolvedHeaders(resolve SecretResolver) (http.Header, error) {
	out := http.Header{}
	if cfg.BearerToken != "" {
		if resolve == nil {
			return nil, errors.New("mcp: bearer_token requires a SecretResolver")
		}
		val, err := resolve(cfg.BearerToken)
		if err != nil {
			return nil, fmt.Errorf("mcp: resolve bearer_token: %w", err)
		}
		out.Set("Authorization", "Bearer "+val)
	}
	for k, ref := range cfg.SecretHeaders {
		if resolve == nil {
			return nil, errors.New("mcp: secret_headers entries require a SecretResolver")
		}
		val, err := resolve(ref)
		if err != nil {
			// The KEY, never the ref's value: a resolver failure on a
			// bad path would otherwise print the path, and on a bad
			// env name the name of a variable holding a token.
			return nil, fmt.Errorf("mcp: resolve secret header %q: %w", k, err)
		}
		out.Set(k, val)
	}
	for k, v := range cfg.Headers {
		out.Set(k, v)
	}
	return out, nil
}

// Validate refuses a server that is neither shape or both.
//
// Both is the one worth refusing loudly: install runs a command
// before the transport opens, so a config carrying a url AND an
// install would execute something for a server that never spawns —
// a subprocess nobody declared, on a path nobody reads.
func (cfg ServerConfig) Validate() error {
	remote := cfg.Remote()
	local := strings.TrimSpace(cfg.Command) != ""

	switch {
	case remote && local:
		return errors.New("mcp: a server takes either url (remote) or command (stdio), not both")
	case !remote && !local:
		return errors.New("mcp: a server needs a url (remote) or a command (stdio)")
	}
	if remote {
		if len(cfg.Install) > 0 {
			return errors.New("mcp: install is for a stdio server's binary; a remote server spawns nothing")
		}
		if len(cfg.Env) > 0 || len(cfg.SecretEnv) > 0 {
			return errors.New("mcp: env is for a stdio server's process; a remote server takes headers")
		}
		if len(cfg.Networks) > 0 {
			return errors.New("mcp: networks is for a stdio server; a remote server's url is its allowlist")
		}
		if _, err := url.Parse(cfg.URL); err != nil {
			return fmt.Errorf("mcp: url %q: %w", cfg.URL, err)
		}
	}
	if local && (len(cfg.Headers) > 0 || len(cfg.SecretHeaders) > 0 || cfg.BearerToken != "") {
		return errors.New("mcp: headers are for a remote server; a stdio server takes env")
	}
	// Two ways to set one header is a collision waiting to be
	// debugged from the far side of an HTTP 401.
	if cfg.BearerToken != "" {
		for _, m := range []map[string]string{cfg.Headers, cfg.SecretHeaders} {
			for k := range m {
				if strings.EqualFold(k, "Authorization") {
					return errors.New("mcp: bearer_token and an Authorization header both set; keep one")
				}
			}
		}
	}
	return nil
}
