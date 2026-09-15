package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jmylchreest/lobslaw/internal/archive"
)

func TestRepositoryIndependentEncryptedGenerationsAndRetention(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	repo := Repository{Path: t.TempDir()}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for i, id := range []string{"old", "pinned", "recent", "latest"} {
		snapshot := archive.Snapshot{
			Manifest: archive.Manifest{SnapshotID: id, CreatedAt: now.Add(time.Duration(i-3) * 24 * time.Hour)},
			Records:  []archive.Record{{Kind: "documents", ID: "v", Data: []byte(`{"id":"v","text":"backup content"}`)}},
		}
		if _, err := repo.Create(snapshot, identity.Recipient()); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Create(snapshot, identity.Recipient()); err == nil {
			t.Fatal("overwrote immutable generation")
		}
	}
	if err := repo.Pin("pinned", true); err != nil {
		t.Fatal(err)
	}
	plan, err := repo.PlanPrune(Retention{KeepLast: 1, KeepWithin: 36 * time.Hour}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Manifest.SnapshotID != "old" {
		t.Fatalf("retention did not keep age/count union and pin: %+v", plan)
	}
	listed, err := repo.List()
	if err != nil || len(listed) != 4 {
		t.Fatalf("preview deleted backups: %v", err)
	}
	if err := repo.Prune(plan); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"pinned", "recent", "latest"} {
		snapshot, err := repo.Read(id, identity)
		if err != nil || len(snapshot.Records) != 1 || snapshot.Manifest.SnapshotID != id {
			t.Fatalf("generation %s is not independently restorable: %v", id, err)
		}
	}
}

func TestRepositoryRejectsTamperingAndRechecksPinsBeforePruning(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	repo := Repository{Path: t.TempDir()}
	now := time.Now().UTC()
	snapshot := archive.Snapshot{Manifest: archive.Manifest{SnapshotID: "backup", CreatedAt: now.Add(-48 * time.Hour)}}
	if _, err := repo.Create(snapshot); err == nil {
		t.Fatal("created unencrypted backup")
	}
	if _, err := repo.Create(snapshot, identity.Recipient()); err != nil {
		t.Fatal(err)
	}
	plan, err := repo.PlanPrune(Retention{KeepWithin: time.Hour}, now)
	if err != nil || len(plan) != 1 {
		t.Fatalf("prune plan: %v", err)
	}
	if err := repo.Pin("backup", true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Prune(plan); err == nil {
		t.Fatal("stale prune plan ignored a new pin")
	}
	if err := os.WriteFile(filepath.Join(repo.Path, "backup", "archive.age"), []byte("truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Read("backup", identity); err == nil {
		t.Fatal("read tampered backup")
	}
	if _, err := repo.List(); err == nil {
		t.Fatal("listed corrupt generation as restorable")
	}
	if _, err := repo.Read("../outside", identity); err == nil {
		t.Fatal("accepted path traversal")
	}
}
