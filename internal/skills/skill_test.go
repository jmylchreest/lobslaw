package skills

import (
	"crypto/sha256"
	"encoding/hex"
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

// A hand-authored manifest (SKILL.md frontmatter, e.g.) may declare no
// version at all. Parse must accept it and default in-memory, without
// rewriting the file: SHA256 is checked against the bytes as written,
// not against a re-serialised manifest, which is what would break a
// signature over the original.
func TestParseDefaultsAMissingVersionWithoutTouchingTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeHandler(t, dir, "h.sh", "#!/bin/sh\n")
	manifest := "name: weather\nruntime: bash\nhandler: h.sh\n"
	writeManifest(t, dir, manifest)

	skill, err := Parse(dir)
	if err != nil {
		t.Fatal(err)
	}
	if skill.Manifest.Version != DefaultVersion {
		t.Errorf("version: got %q, want %q", skill.Manifest.Version, DefaultVersion)
	}

	onDisk, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != manifest {
		t.Fatalf("manifest.yaml was rewritten: got %q, want %q", onDisk, manifest)
	}
	want := sha256.Sum256([]byte(manifest))
	if skill.SHA256 != hex.EncodeToString(want[:]) {
		t.Error("SHA256 was computed over something other than the untouched manifest bytes")
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
