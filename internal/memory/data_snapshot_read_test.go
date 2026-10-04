package memory

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

func snapshotInspectionFixture(t *testing.T) (string, crypto.Key, string) {
	t.Helper()
	dir, key := legacyDataDirectory(t)
	store, err := raft.NewFileSnapshotStore(filepath.Join(dir, SnapshotDir), retainedSnapshots, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := raft.NewInmemTransport("fixture")
	sink, err := store.Create(raft.SnapshotVersionMax, 1, 7, raft.Configuration{Servers: []raft.Server{{ID: "fixture", Address: "fixture", Suffrage: raft.Voter}}}, 1, transport)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, key, filepath.Join(dir, SnapshotDir, "snapshots", sink.ID(), "state.bin")
}

func TestInspectSnapshotsRejectsSymlinkWithoutWriting(t *testing.T) {
	dir, key, _ := snapshotInspectionFixture(t)
	target := filepath.Join(t.TempDir(), "unrelated")
	if err := os.WriteFile(target, []byte("must survive"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, SnapshotDir, "snapshots", "permTest")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, inspectErr := InspectData(context.Background(), dir, dataformat.LegacyMain, key)
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "must survive" {
		t.Fatal("inspection truncated unrelated symlink target")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("inspection changed source symlink: %v", err)
	}
	if inspectErr == nil {
		t.Fatal("accepted source symlink")
	}
}

func TestInspectSnapshotsReadOnlySource(t *testing.T) {
	dir, key, _ := snapshotInspectionFixture(t)
	repo := filepath.Join(dir, SnapshotDir, "snapshots")
	sentinel := filepath.Join(repo, "permTest")
	if err := os.WriteFile(sentinel, []byte("source sentinel"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := filepath.Walk(repo, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0400)
		if info.IsDir() {
			mode = 0500
		}
		return os.Chmod(path, mode)
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.Walk(repo, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	})
	before, err := os.Stat(repo)
	if err != nil {
		t.Fatal(err)
	}
	report, err := InspectData(context.Background(), dir, dataformat.LegacyMain, key)
	if err != nil {
		t.Fatal(err)
	}
	if report.Snapshots != 1 {
		t.Fatalf("snapshots=%d", report.Snapshots)
	}
	after, err := os.Stat(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("inspection changed source directory")
	}
	raw, err := os.ReadFile(sentinel)
	if err != nil || string(raw) != "source sentinel" {
		t.Fatalf("source sentinel changed: %q %v", raw, err)
	}
}

func TestInspectSnapshotsStillChecksChecksum(t *testing.T) {
	dir, key, statePath := snapshotInspectionFixture(t)
	f, err := os.OpenFile(statePath, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("corruption")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectData(context.Background(), dir, dataformat.LegacyMain, key); err == nil || !strings.Contains(err.Error(), "CRC mismatch") {
		t.Fatalf("expected checksum rejection, got %v", err)
	}
}
