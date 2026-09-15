package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestArchiveExportEncryptedAndDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	dbpath := filepath.Join(dir, "state.db")
	out := filepath.Join(dir, "backup.age")
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(dbpath, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(&lobslawv1.ScheduledTaskRecord{Id: "cron", Schedule: "0 9 * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketScheduledTasks, "cron", raw); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOBSLAW_MEMORY_KEY", base64.StdEncoding.EncodeToString(key[:]))
	t.Setenv("LOBSLAW_CONFIG", "")
	t.Setenv("LOBSLAW_ENV", filepath.Join(dir, "missing.env"))
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--offline", "--state-db", dbpath, "--out", out, "--recipient", id.Recipient().String()}
	if err := archiveExport(args); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	snap, err := archive.Read(f, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Manifest.Counts["scheduled-tasks"] != 1 {
		t.Fatal("schedule missing")
	}
	if err := archiveExport(args); err == nil {
		t.Fatal("overwrote existing backup")
	}
}

func TestArchiveExportRequiresExplicitEncryptionAndOfflineMode(t *testing.T) {
	for _, args := range [][]string{{"--offline", "--out", filepath.Join(t.TempDir(), "backup")}, {"--plaintext", "--out", "unused"}} {
		if err := archiveExport(args); err == nil {
			t.Fatalf("accepted unsafe or unsupported invocation: %v", args)
		}
	}
}
