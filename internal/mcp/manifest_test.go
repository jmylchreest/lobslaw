package mcp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadManifestHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	writeManifest(t, path, `{
		"mcpServers": {
			"fs": {"command": "mcp-fs", "args": ["--root", "/"]}
		}
	}`)
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	fs, ok := m.MCPServers["fs"]
	if !ok {
		t.Fatal("fs server missing")
	}
	if fs.Command != "mcp-fs" {
		t.Errorf("command: %q", fs.Command)
	}
}

func TestLoadManifestRejectsEmptyCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	writeManifest(t, path, `{"mcpServers":{"x":{"command":""}}}`)
	_, err := LoadManifest(path)
	if err == nil {
		t.Error("empty command should fail")
	}
}

func TestLoadManifestAllowsDisabledWithoutCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	writeManifest(t, path, `{"mcpServers":{"x":{"disabled":true}}}`)
	_, err := LoadManifest(path)
	if err != nil {
		t.Errorf("disabled server should tolerate empty command; got %v", err)
	}
}

func TestLoadManifestRejectsSeparatorInName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	writeManifest(t, path, `{"mcpServers":{"foo/bar":{"command":"x"}}}`)
	_, err := LoadManifest(path)
	if err == nil {
		t.Error("name with / should fail")
	}
}

func TestDiscoverManifestsHappyPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"alpha", "bravo"} {
		writeManifest(t, filepath.Join(root, name, ".mcp.json"),
			`{"mcpServers":{"srv":{"command":"mcp-`+name+`"}}}`)
	}
	// Non-MCP plugin dir — no .mcp.json.
	_ = os.Mkdir(filepath.Join(root, "charlie"), 0o755)

	list, err := DiscoverManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if !strings.HasSuffix(list[0].PluginDir, "alpha") {
		t.Errorf("expected alpha first; got %q", list[0].PluginDir)
	}
}

func TestDiscoverManifestsMissingRoot(t *testing.T) {
	t.Parallel()
	list, err := DiscoverManifests(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty result for missing root; got %+v", list)
	}
}

func TestResolvedEnvPlainAndSecrets(t *testing.T) {
	t.Parallel()
	cfg := ServerConfig{
		Command: "x",
		Env:     map[string]string{"FOO": "plain"},
		SecretEnv: map[string]string{
			"API_KEY": "env:MCP_KEY",
		},
	}
	resolver := func(ref string) (string, error) {
		if ref == "env:MCP_KEY" {
			return "sshhh", nil
		}
		return "", errors.New("unknown")
	}
	env, err := cfg.ResolvedEnv(resolver)
	if err != nil {
		t.Fatal(err)
	}
	foundFoo, foundKey := false, false
	for _, e := range env {
		if e == "FOO=plain" {
			foundFoo = true
		}
		if e == "API_KEY=sshhh" {
			foundKey = true
		}
	}
	if !foundFoo {
		t.Error("plain env lost")
	}
	if !foundKey {
		t.Error("resolved secret missing")
	}
}

func TestResolvedEnvPlainWinsOnCollision(t *testing.T) {
	t.Parallel()
	cfg := ServerConfig{
		Env:       map[string]string{"K": "plain"},
		SecretEnv: map[string]string{"K": "env:K"},
	}
	env, err := cfg.ResolvedEnv(func(string) (string, error) { return "secret", nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range env {
		if e == "K=secret" {
			t.Error("secret should lose to plain on collision")
		}
	}
}

func TestResolvedEnvRejectsSecretsWithoutResolver(t *testing.T) {
	t.Parallel()
	cfg := ServerConfig{SecretEnv: map[string]string{"X": "env:X"}}
	_, err := cfg.ResolvedEnv(nil)
	if err == nil {
		t.Error("secret_env without resolver should fail")
	}
}

func TestResolvedEnvPropagatesResolverError(t *testing.T) {
	t.Parallel()
	cfg := ServerConfig{SecretEnv: map[string]string{"X": "env:missing"}}
	_, err := cfg.ResolvedEnv(func(_ string) (string, error) {
		return "", errors.New("not found")
	})
	if err == nil {
		t.Error("resolver error should propagate")
	}
}

// The two shapes are mutually exclusive, and "both" is the one worth
// refusing loudly: install runs a command before the transport
// opens, so a url plus an install would execute something for a
// server that never spawns.
func TestServerConfigRefusesMixedShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  ServerConfig
		want string
	}{
		{"both", ServerConfig{URL: "https://x.test/sse", Command: "uvx"}, "not both"},
		{"neither", ServerConfig{}, "needs a url"},
		{"remote with install", ServerConfig{URL: "https://x.test/sse", Install: []string{"uv", "tool"}}, "spawns nothing"},
		{"remote with env", ServerConfig{URL: "https://x.test/sse", Env: map[string]string{"A": "b"}}, "takes headers"},
		{"remote with networks", ServerConfig{URL: "https://x.test/sse", Networks: []string{"x.test"}}, "its allowlist"},
		{"stdio with headers", ServerConfig{Command: "uvx", Headers: map[string]string{"A": "b"}}, "takes env"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatalf("%+v was accepted", tc.cfg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q; want it to mention %q", err, tc.want)
			}
		})
	}
}

// Both shapes on their own are fine.
func TestServerConfigAcceptsEitherShape(t *testing.T) {
	t.Parallel()
	for _, cfg := range []ServerConfig{
		{URL: "https://x.test/sse", SecretHeaders: map[string]string{"Authorization": "env:TOKEN"}},
		{Command: "uvx", Args: []string{"server"}, Networks: []string{"api.example.test"}},
	} {
		if err := cfg.Validate(); err != nil {
			t.Errorf("%+v was refused: %v", cfg, err)
		}
	}
}

// A secret header resolves like every other secret, and a failure
// names the HEADER rather than the ref — a resolver error on a bad
// env name would otherwise print the name of a variable holding a
// token.
func TestResolvedHeadersMergesAndHidesRefs(t *testing.T) {
	t.Parallel()
	cfg := ServerConfig{
		URL:           "https://x.test/sse",
		Headers:       map[string]string{"X-Client": "lobslaw"},
		SecretHeaders: map[string]string{"Authorization": "env:KITCHENOWL_TOKEN"},
	}
	got, err := cfg.ResolvedHeaders(func(ref string) (string, error) {
		if ref == "env:KITCHENOWL_TOKEN" {
			return "Bearer live-token", nil
		}
		return "", fmt.Errorf("unknown ref")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer live-token" || got.Get("X-Client") != "lobslaw" {
		t.Fatalf("headers = %v", got)
	}

	_, err = ServerConfig{URL: "https://x.test/sse",
		SecretHeaders: map[string]string{"Authorization": "env:MISSING"}}.
		ResolvedHeaders(func(string) (string, error) { return "", fmt.Errorf("no such variable") })
	if err == nil || !strings.Contains(err.Error(), "Authorization") {
		t.Errorf("err = %v; want it to name the header", err)
	}
	if err != nil && strings.Contains(err.Error(), "MISSING") {
		t.Errorf("err leaks the ref: %v", err)
	}
}

// The secret is the TOKEN, not the header. Storing "Bearer " in the
// vault would make the entry a header value rather than a credential,
// and wrong for anything else that wants the same token.
func TestBearerTokenBecomesAnAuthorizationHeader(t *testing.T) {
	t.Parallel()
	cfg := ServerConfig{URL: "https://x.test/sse", BearerToken: "env:TOKEN"}
	got, err := cfg.ResolvedHeaders(func(string) (string, error) { return "eyJraw", nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer eyJraw" {
		t.Fatalf("Authorization = %q", got.Get("Authorization"))
	}
}

// Two ways to set one header is a collision debugged from the far
// side of an HTTP 401.
func TestBearerTokenAndAuthorizationHeaderCollide(t *testing.T) {
	t.Parallel()
	for _, cfg := range []ServerConfig{
		{URL: "https://x.test/sse", BearerToken: "env:T", Headers: map[string]string{"Authorization": "Basic x"}},
		{URL: "https://x.test/sse", BearerToken: "env:T", SecretHeaders: map[string]string{"authorization": "env:OTHER"}},
	} {
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "keep one") {
			t.Errorf("%+v: err = %v; want the collision refused", cfg, err)
		}
	}
}

// An API wanting a raw key in Authorization — some do — must stay
// expressible without fighting a prefix it never asked for.
func TestRawAuthorizationHeaderIsLeftAlone(t *testing.T) {
	t.Parallel()
	cfg := ServerConfig{URL: "https://x.test/sse",
		SecretHeaders: map[string]string{"Authorization": "env:RAW"}}
	got, err := cfg.ResolvedHeaders(func(string) (string, error) { return "raw-key-no-scheme", nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "raw-key-no-scheme" {
		t.Fatalf("Authorization = %q; a verbatim header was rewritten", got.Get("Authorization"))
	}
}
