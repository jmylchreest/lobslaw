package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jmylchreest/lobslaw/internal/archive"
)

// legacyManifestEntry mirrors the unexported tar entry name internal/archive
// uses for its manifest, so a test can locate and rewrite it byte for byte.
const legacyManifestEntry = "manifest.json"

// rewriteManifestSchema rewrites the schema_version recorded in an
// unencrypted archive's manifest entry to version, simulating a generation
// written by a build whose archive.SchemaVersion constant held that value. It
// returns the rewritten archive bytes and the manifest as archive.Read will
// decode it, so a caller can build the on-disk generation manifest from the
// same values.
func rewriteManifestSchema(t *testing.T, plain []byte, version int) ([]byte, archive.Manifest) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	var out bytes.Buffer
	zipped := gzip.NewWriter(&out)
	tw := tar.NewWriter(zipped)
	var manifest archive.Manifest
	from := fmt.Sprintf(`"schema_version":%d`, archive.SchemaVersion)
	to := fmt.Sprintf(`"schema_version":%d`, version)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == legacyManifestEntry {
			data = []byte(strings.Replace(string(data), from, to, 1))
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
		}
		h.Size = int64(len(data))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zipped.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), manifest
}

// writeLegacyGeneration writes a generation directory whose manifest declares
// version, built from a real encrypted archive so List, Read, Pin and
// PlanPrune all exercise the same on-disk shape a schema-1 backup left behind.
func writeLegacyGeneration(t *testing.T, repo Repository, snapshot archive.Snapshot, recipient age.Recipient, version int) {
	t.Helper()
	var plain bytes.Buffer
	if err := archive.Write(&plain, snapshot); err != nil {
		t.Fatal(err)
	}
	legacy, manifest := rewriteManifestSchema(t, plain.Bytes(), version)
	if manifest.SchemaVersion != version {
		t.Fatalf("rewritten manifest schema = %d, want %d", manifest.SchemaVersion, version)
	}
	dir := filepath.Join(repo.Path, snapshot.Manifest.SnapshotID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(dir, "archive.age"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	encrypted, err := age.Encrypt(io.MultiWriter(file, hash), recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encrypted.Write(legacy); err != nil {
		t.Fatal(err)
	}
	if err := encrypted.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	generation := Generation{Manifest: manifest, SHA256: hex.EncodeToString(hash.Sum(nil))}
	raw, err := json.Marshal(generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryServesSchema1GenerationsAndRefusesUnsupportedOnes(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()

	t.Run("schema 1 lists, reads, pins and is skipped by prune once pinned", func(t *testing.T) {
		t.Parallel()
		repo := Repository{Path: t.TempDir()}
		snapshot := archive.Snapshot{
			Manifest: archive.Manifest{SnapshotID: "legacy", CreatedAt: now.Add(-2 * time.Hour)},
			Records:  []archive.Record{{Kind: "documents", ID: "v", Data: []byte(`{"id":"v","text":"legacy content"}`)}},
		}
		writeLegacyGeneration(t, repo, snapshot, identity.Recipient(), archive.MinSchemaVersion)

		listed, err := repo.List()
		if err != nil || len(listed) != 1 || listed[0].Manifest.SchemaVersion != archive.MinSchemaVersion {
			t.Fatalf("schema %d generation not listed: %+v, %v", archive.MinSchemaVersion, listed, err)
		}
		read, err := repo.Read("legacy", identity)
		if err != nil || len(read.Records) != 1 {
			t.Fatalf("schema %d generation not readable: %+v, %v", archive.MinSchemaVersion, read, err)
		}
		if err := repo.Pin("legacy", true); err != nil {
			t.Fatalf("schema %d generation not pinnable: %v", archive.MinSchemaVersion, err)
		}
		plan, err := repo.PlanPrune(Retention{KeepWithin: time.Hour}, now)
		if err != nil {
			t.Fatalf("prune plan: %v", err)
		}
		if len(plan) != 0 {
			t.Fatalf("pinned schema %d generation was planned for removal: %+v", archive.MinSchemaVersion, plan)
		}
	})

	t.Run("unsupported schemas are refused", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name    string
			version int
		}{
			{"below minimum", archive.MinSchemaVersion - 1},
			{"above current", archive.SchemaVersion + 1},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				repo := Repository{Path: t.TempDir()}
				snapshot := archive.Snapshot{
					Manifest: archive.Manifest{SnapshotID: "unsupported", CreatedAt: now},
					Records:  []archive.Record{{Kind: "documents", ID: "v", Data: []byte(`{"id":"v"}`)}},
				}
				writeLegacyGeneration(t, repo, snapshot, identity.Recipient(), c.version)
				if _, err := repo.Read("unsupported", identity); err == nil {
					t.Fatalf("schema %d generation accepted", c.version)
				}
				if _, err := repo.List(); err == nil {
					t.Fatalf("schema %d generation did not fail listing", c.version)
				}
			})
		}
	})
}

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
