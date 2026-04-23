package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeHandler(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestParseHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "handler.py", "print('hi')")
	writeManifest(t, dir, `
name: greeter
version: 1.0.0
runtime: python
handler: handler.py
description: A simple hello
storage:
  - label: shared
    mode: read
`)
	skill, err := Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Name() != "greeter" {
		t.Errorf("Name: %q", skill.Name())
	}
	if skill.Manifest.Version != "1.0.0" || skill.Manifest.Runtime != RuntimePython {
		t.Errorf("manifest shape: %+v", skill.Manifest)
	}
	if skill.HandlerPath != filepath.Join(dir, "handler.py") {
		t.Errorf("handler path: %q", skill.HandlerPath)
	}
	if skill.SHA256 == "" {
		t.Error("sha empty")
	}
	if len(skill.Manifest.Storage) != 1 || skill.Manifest.Storage[0].Label != "shared" {
		t.Errorf("storage: %+v", skill.Manifest.Storage)
	}
}

func TestParseRequiresAbsDir(t *testing.T) {
	t.Parallel()
	_, err := Parse("relative/dir")
	if err == nil {
		t.Error("relative dir should fail")
	}
}

func TestParseRejectsMissingManifest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := Parse(dir)
	if err == nil {
		t.Error("missing manifest should fail")
	}
}

func TestParseRejectsMissingHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeManifest(t, dir, `
name: s
version: 1.0.0
runtime: bash
handler: missing.sh
`)
	_, err := Parse(dir)
	if err == nil {
		t.Error("missing handler file should fail")
	}
}

func TestValidateRejectsUnknownRuntime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h", "")
	writeManifest(t, dir, `
name: s
version: 1.0.0
runtime: ruby
handler: h
`)
	_, err := Parse(dir)
	if err == nil || !strings.Contains(err.Error(), "runtime") {
		t.Errorf("want runtime error; got %v", err)
	}
}

func TestValidateRejectsTraversalHandler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h", "")
	writeManifest(t, dir, `
name: s
version: 1.0.0
runtime: bash
handler: ../h
`)
	_, err := Parse(dir)
	if err == nil {
		t.Error("../ handler should be rejected")
	}
}

func TestValidateDefaultsStorageMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h.sh", "")
	writeManifest(t, dir, `
name: s
version: 1.0.0
runtime: bash
handler: h.sh
storage:
  - label: shared
`)
	s, err := Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Manifest.Storage[0].Mode != StorageRead {
		t.Errorf("default mode should be read; got %q", s.Manifest.Storage[0].Mode)
	}
}

func TestValidateRejectsBadMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h.sh", "")
	writeManifest(t, dir, `
name: s
version: 1.0.0
runtime: bash
handler: h.sh
storage:
  - label: shared
    mode: delete
`)
	_, err := Parse(dir)
	if err == nil {
		t.Error("delete mode should be rejected")
	}
}

func TestValidateRejectsNameWithSeparator(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h", "")
	writeManifest(t, dir, `
name: foo/bar
version: 1.0.0
runtime: bash
handler: h
`)
	_, err := Parse(dir)
	if err == nil {
		t.Error("name with / should be rejected")
	}
}
