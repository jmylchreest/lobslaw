package mcp

import (
	"errors"
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
