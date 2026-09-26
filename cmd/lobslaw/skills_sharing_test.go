package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sharing"
)

func TestSkillsShareOfflinePublishInspectAndSign(t *testing.T) {
	dir := t.TempDir()
	skill := filepath.Join(dir, "skill")
	if err := os.Mkdir(skill, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "manifest.yaml"), []byte("name: sample\nversion: 1.0.0\nruntime: prose\nbody: SKILL.md\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("Instructions"), 0600); err != nil {
		t.Fatal(err)
	}
	ref := "file:" + filepath.Join(dir, "skill.share")
	if err := skillsPublish([]string{"--dir", skill, "--to", ref}); err != nil {
		t.Fatal(err)
	}
	if err := skillsInspect([]string{ref}); err != nil {
		t.Fatal(err)
	}
	if err := skillsPublish([]string{"--dir", skill, "--to", ref}); err == nil {
		t.Fatal("overwrote published artifact")
	}
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(key)), 0600); err != nil {
		t.Fatal(err)
	}
	signedRef := "file:" + filepath.Join(dir, "signed.share")
	if err := skillsSign([]string{"--key", keyPath, "--publisher", "alice", "--to", signedRef, ref}); err != nil {
		t.Fatal(err)
	}
	a, err := (sharing.FileBackend{}).Fetch(context.Background(), signedRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := sharing.Verify(a, map[string]ed25519.PublicKey{"alice": pub}, true); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsFetchClawhubToPortableFile(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("---\nname: demo\n---\nUse demo.\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(buf.Bytes()) }))
	defer srv.Close()
	t.Setenv("LOBSLAW_CLAWHUB_BASE_URL", srv.URL)
	ref := "file:" + filepath.Join(t.TempDir(), "demo.share")
	if err := skillsFetch([]string{"clawhub:demo", "--to", ref}); err != nil {
		t.Fatal(err)
	}
	a, err := (sharing.FileBackend{}).Fetch(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if a.Package().Origin == nil || a.Package().Name != "demo" {
		t.Fatal("fetch lost source metadata")
	}
	if err := skillsInspect([]string{ref}); err != nil {
		t.Fatal(err)
	}
}
