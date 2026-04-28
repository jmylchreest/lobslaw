package clawhub

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// makeZipBundle builds an in-memory zip with the supplied entries
// (path → bytes). Empty bytes makes a directory-like entry.
func makeZipBundle(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fh.SetMode(0o644)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcessBundleClawhubFormat(t *testing.T) {
	skillMD := `---
name: gog
description: Google Workspace CLI
homepage: https://gogcli.sh
metadata: {"clawdbot":{"emoji":"🎮","requires":{"bins":["gog"]},"install":[{"id":"brew","kind":"brew","formula":"steipete/tap/gogcli","bins":["gog"]}]}}
---

# gog

Use gog for Gmail/Calendar/Drive.
`
	bundle := makeZipBundle(t, map[string][]byte{
		"SKILL.md":   []byte(skillMD),
		"_meta.json": []byte(`{"slug":"gog","version":"1.0.0"}`),
	})
	dir := t.TempDir()
	res, err := ProcessBundle(bundle, dir)
	if err != nil {
		t.Fatalf("ProcessBundle: %v", err)
	}
	if res.Format != "clawhub" {
		t.Errorf("format: %q", res.Format)
	}
	if res.Name != "gog" {
		t.Errorf("name: %q", res.Name)
	}
	if len(res.RequiresBins) != 1 || res.RequiresBins[0] != "gog" {
		t.Errorf("requires: %v", res.RequiresBins)
	}
	if len(res.InstallSpecs) != 1 || res.InstallSpecs[0].Manager != "brew" {
		t.Errorf("install specs: %v", res.InstallSpecs)
	}
	if !strings.Contains(res.Prose, "# gog") {
		t.Errorf("prose: %q", res.Prose)
	}

	// Synthetic manifest written
	if _, err := os.Stat(filepath.Join(dir, "manifest.yaml")); err != nil {
		t.Errorf("manifest.yaml not written: %v", err)
	}
	// Original SKILL.md preserved
	if _, err := os.Stat(filepath.Join(dir, "SKILL.md")); err != nil {
		t.Errorf("SKILL.md not preserved: %v", err)
	}
	// Handler shim
	info, err := os.Stat(filepath.Join(dir, "handler.sh"))
	if err != nil {
		t.Errorf("handler.sh not written: %v", err)
	} else if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("handler.sh not executable: %v", info.Mode())
	}

	// Synthetic manifest parses + has one tool named for the skill
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name           string   `yaml:"name"`
		Tools          []map[string]any `yaml:"tools"`
		RequiresBinary []string `yaml:"requires_binary"`
	}
	if err := yaml.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse synthetic manifest: %v", err)
	}
	if m.Name != "gog" {
		t.Errorf("manifest name: %q", m.Name)
	}
	if len(m.Tools) != 1 {
		t.Fatalf("tools: %v", m.Tools)
	}
	if got, _ := m.Tools[0]["name"].(string); got != "gog" {
		t.Errorf("tool name: %q", got)
	}
	if len(m.RequiresBinary) != 1 || m.RequiresBinary[0] != "gog" {
		t.Errorf("requires_binary: %v", m.RequiresBinary)
	}
}

func TestProcessBundleNativeFormat(t *testing.T) {
	manifest := []byte(`schema_version: 1
name: my-skill
runtime: bash
handler: handler.sh
`)
	bundle := makeZipBundle(t, map[string][]byte{
		"manifest.yaml": manifest,
		"handler.sh":    []byte("#!/bin/sh\necho hi"),
	})
	dir := t.TempDir()
	res, err := ProcessBundle(bundle, dir)
	if err != nil {
		t.Fatalf("ProcessBundle: %v", err)
	}
	if res.Format != "native" {
		t.Errorf("format: %q", res.Format)
	}
	if res.Name != "my-skill" {
		t.Errorf("name: %q", res.Name)
	}
	if len(res.RequiresBins) != 0 {
		t.Errorf("requires bins should be empty for native: %v", res.RequiresBins)
	}
}

func TestProcessBundleNativeWinsWhenBothPresent(t *testing.T) {
	bundle := makeZipBundle(t, map[string][]byte{
		"manifest.yaml": []byte("schema_version: 1\nname: native-wins\nruntime: bash\nhandler: h.sh\n"),
		"SKILL.md":      []byte("---\nname: clawhub-loses\n---\nbody\n"),
	})
	dir := t.TempDir()
	res, err := ProcessBundle(bundle, dir)
	if err != nil {
		t.Fatalf("ProcessBundle: %v", err)
	}
	if res.Format != "native" {
		t.Errorf("format: %q (manifest.yaml should win)", res.Format)
	}
	if res.Name != "native-wins" {
		t.Errorf("name: %q", res.Name)
	}
}

func TestProcessBundleTarGz(t *testing.T) {
	manifest := []byte("schema_version: 1\nname: tar-skill\nruntime: bash\nhandler: h.sh\n")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(buildSimpleTar(t, "manifest.yaml", manifest)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	res, err := ProcessBundle(buf.Bytes(), dir)
	if err != nil {
		t.Fatalf("ProcessBundle: %v", err)
	}
	if res.Format != "native" || res.Name != "tar-skill" {
		t.Errorf("res: %+v", res)
	}
}

func TestProcessBundleRejectsUnknownFormat(t *testing.T) {
	_, err := ProcessBundle([]byte("not a bundle"), t.TempDir())
	if err == nil {
		t.Fatal("expected unknown-format error")
	}
}

func TestProcessBundleRejectsZipSlip(t *testing.T) {
	bundle := makeZipBundle(t, map[string][]byte{
		"../escape.txt": []byte("evil"),
	})
	_, err := ProcessBundle(bundle, t.TempDir())
	if err == nil {
		t.Fatal("expected zip slip rejection")
	}
}

// TestProcessBundleRealGogBundle smoke-tests against the actual
// clawhub.ai-served gog bundle (cached at /tmp/gog-bundle.zip if a
// previous session has it). Skipped when the file isn't present so
// CI without network access stays green.
func TestProcessBundleRealGogBundle(t *testing.T) {
	bundle, err := os.ReadFile("/tmp/gog-bundle.zip")
	if err != nil {
		t.Skip("no /tmp/gog-bundle.zip — run `curl -sL 'https://wry-manatee-359.convex.site/api/v1/download?slug=gog' -o /tmp/gog-bundle.zip` to enable")
	}
	res, err := ProcessBundle(bundle, t.TempDir())
	if err != nil {
		t.Fatalf("ProcessBundle on real gog bundle: %v", err)
	}
	if res.Format != "clawhub" {
		t.Errorf("format: %q (gog is clawhub-format)", res.Format)
	}
	if res.Name != "gog" {
		t.Errorf("name: %q", res.Name)
	}
	if len(res.RequiresBins) != 1 || res.RequiresBins[0] != "gog" {
		t.Errorf("requires bins: %v", res.RequiresBins)
	}
	if len(res.InstallSpecs) != 1 || res.InstallSpecs[0].Manager != "brew" {
		t.Errorf("install specs: %v", res.InstallSpecs)
	}
	if !strings.Contains(res.Prose, "gog auth credentials") {
		t.Errorf("prose missing setup instructions: %q", res.Prose[:min(200, len(res.Prose))])
	}
}

func TestProcessBundleClawhubNoBinsNoSpecs(t *testing.T) {
	skillMD := `---
name: prose-only
description: A skill with prose but no binary requirements
---

# prose-only

Just markdown, no host requirements.
`
	bundle := makeZipBundle(t, map[string][]byte{
		"SKILL.md": []byte(skillMD),
	})
	dir := t.TempDir()
	res, err := ProcessBundle(bundle, dir)
	if err != nil {
		t.Fatalf("ProcessBundle: %v", err)
	}
	if res.Format != "clawhub" {
		t.Errorf("format: %q", res.Format)
	}
	if len(res.RequiresBins) != 0 {
		t.Errorf("expected no bins required, got %v", res.RequiresBins)
	}
}

func buildSimpleTar(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
