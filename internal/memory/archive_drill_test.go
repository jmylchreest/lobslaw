package memory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"filippo.io/age"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/backup"
	"github.com/jmylchreest/lobslaw/internal/embedder"
)

// Optional private fixture drill. The committed test contains neither personal
// records nor keys; regular CI uses the synthetic round-trip tests.
func TestArchivePrivateFixtureDrill(t *testing.T) {
	path := os.Getenv("LOBSLAW_ARCHIVE_DRILL")
	if path == "" {
		t.Skip("set LOBSLAW_ARCHIVE_DRILL and LOBSLAW_ARCHIVE_IDENTITY for a private restore drill")
	}
	runArchiveFixtureDrill(t, path, os.Getenv("LOBSLAW_ARCHIVE_IDENTITY"), os.Getenv("LOBSLAW_ARCHIVE_DESTINATION"), os.Getenv("LOBSLAW_ARCHIVE_MODEL"), os.Getenv("LOBSLAW_ARCHIVE_REPLACE_ORIGINAL") == "1")
}

// Ordinary CI exercises encrypted alongside -> original replacement -> retry.
// All records and keys are synthetic and scoped to this test's temporary directory.
func TestArchiveReplaceOriginalEncryptedDrill(t *testing.T) {
	existing, incoming, _ := archiveOriginalFixture(t)
	var original []archive.Record
	for _, record := range existing {
		if record.ID == "chat" || record.ID == sessionMessageKey("chat", 10) || record.ID == "job" {
			original = append(original, record)
		}
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "identity")
	if err := os.WriteFile(keyPath, []byte(identity.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name string, records []archive.Record) string {
		var encrypted bytes.Buffer
		if err := archive.Write(&encrypted, archive.Snapshot{Manifest: archive.Manifest{SnapshotID: name, CreatedAt: time.Now()}, Records: records}, identity.Recipient()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name+".age")
		if err := os.WriteFile(path, encrypted.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	runArchiveFixtureDrill(t, write("source", incoming), keyPath, write("destination", original), "", true)
}

func runArchiveFixtureDrill(t *testing.T, path, identityPath, destinationPath, modelPath string, replaceOriginal bool) {
	t.Helper()
	keyFile, err := os.Open(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	identities, err := age.ParseIdentities(keyFile)
	_ = keyFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := readArchiveDrill(t, path, identities)
	opts := ArchiveImportOptions{
		Owners: make(map[string]string), SourceTimezone: "UTC",
	}
	archiveDrillOwners(t, snapshot.Records, opts.Owners)
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	var destination ReembedEmbedder = stubEmbedder{model: "drill-model"}
	if modelPath != "" {
		encoder, err := embedder.Open(modelPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = encoder.Close() })
		destination = archiveDrillEmbedder{encoder: encoder, model: filepath.Base(modelPath)}
	}
	if destinationPath != "" {
		baseline := readArchiveDrill(t, destinationPath, identities)
		archiveDrillOwners(t, baseline.Records, opts.Owners)
		if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), baseline.Records, opts, destination); err != nil {
			t.Fatal(err)
		}
		preview, err := PlanArchiveImport(mustArchiveRecords(t, fsm.Store()), snapshot.Records, opts)
		if err != nil {
			t.Fatal(err)
		}
		opts.SourceID = "private-drill-source"
		opts.Alongside = archiveDrillConflictGroups(preview.Conflicts)
		t.Logf("isolated merge: %d baseline records, %d explicit alongside selections", len(baseline.Records), len(opts.Alongside))
	}
	result, err := ApplyArchiveImport(ctx, node, fsm.Store(), snapshot.Records, opts, destination)
	if err != nil {
		t.Fatalf("private fixture import: completed %d/%d: %v", result.Completed, result.Total, err)
	}
	if result.Completed != len(snapshot.Records) {
		t.Fatal("incomplete restore")
	}
	restored, err := fsm.Store().ArchiveRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanArchiveImport(restored, snapshot.Records, opts)
	if err != nil || len(plan.Duplicates) != len(snapshot.Records) || len(plan.Additions) != 0 {
		t.Fatalf("round trip differs: additions=%d duplicates=%d conflicts=%d err=%v",
			len(plan.Additions), len(plan.Duplicates), len(plan.Conflicts), err)
	}
	repeated, err := ApplyArchiveImport(ctx, node, fsm.Store(), snapshot.Records, opts, nil)
	if err != nil || repeated.Applied != 0 || repeated.Completed != len(snapshot.Records) {
		t.Fatalf("repeat import: %+v, %v", repeated, err)
	}
	if replaceOriginal {
		opts.ReplaceOriginal = opts.Alongside
		opts.Alongside = nil
		opts.BackupDigest = archiveDrillBackup(t, restored, identities)
		preview, err := PlanArchiveImport(restored, snapshot.Records, opts)
		if err != nil || len(preview.Replaced) == 0 || len(preview.Replaced) != len(opts.ReplaceOriginal) || len(preview.Removed) == 0 {
			t.Fatalf("replacement preview: %+v %v", preview, err)
		}
		replacement, err := ApplyArchiveImport(ctx, node, fsm.Store(), snapshot.Records, opts, destination)
		if err != nil || replacement.Applied == 0 {
			t.Fatalf("replacement: %+v %v", replacement, err)
		}
		t.Logf("replaced original groups: %d source records applied", replacement.Applied)
		opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
		if err != nil {
			t.Fatal(err)
		}
		repeated, err = ApplyArchiveImport(ctx, node, fsm.Store(), snapshot.Records, opts, destination)
		if err != nil || repeated.Applied != 0 {
			t.Fatalf("replacement repeat: %+v %v", repeated, err)
		}
	}
	t.Logf("restored %d records through Raft into a fresh key; destination embeddings rebuilt", result.Completed)
}

func archiveDrillOwners(t *testing.T, records []archive.Record, owners map[string]string) {
	t.Helper()
	for _, record := range records {
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		value := msg.ProtoReflect()
		for _, name := range []protoreflect.Name{"owner", "user_id"} {
			field := value.Descriptor().Fields().ByName(name)
			if field != nil {
				owner := value.Get(field).String()
				if owner != "" {
					owners[owner] = owner
				}
			}
		}
	}
}

type archiveDrillEmbedder struct {
	encoder *embedder.Encoder
	model   string
}

func (e archiveDrillEmbedder) Model() string { return e.model }

func (e archiveDrillEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.encoder.Encode(text), nil
}

func archiveDrillBackup(t *testing.T, records []archive.Record, identities []age.Identity) string {
	t.Helper()
	// Use a real encrypted, verified and pinned pre-replacement backup.
	repo := backup.Repository{Path: t.TempDir()}
	var recipients []age.Recipient
	for _, identity := range identities {
		if key, ok := identity.(*age.X25519Identity); ok {
			recipients = append(recipients, key.Recipient())
		}
	}
	generation, err := repo.Create(archive.Snapshot{Manifest: archive.Manifest{SnapshotID: "before-replacement", CreatedAt: time.Now()}, Records: records}, recipients...)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := repo.Read(generation.Manifest.SnapshotID, identities...)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Pin(generation.Manifest.SnapshotID, true); err != nil {
		t.Fatal(err)
	}
	digest, err := ArchiveStateDigest(verified.Records)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func archiveDrillConflictGroups(conflicts []ArchiveRecordRef) []ArchiveRecordRef {
	seen := make(map[ArchiveRecordRef]bool)
	var selections []ArchiveRecordRef
	for _, ref := range conflicts {
		group := archiveRecordGroup(archiveRecordKey{ref.Kind, ref.ID})
		ref = ArchiveRecordRef{Kind: group.kind, ID: group.id}
		if !seen[ref] {
			selections = append(selections, ref)
			seen[ref] = true
		}
	}
	return selections
}

func readArchiveDrill(t *testing.T, path string, identities []age.Identity) archive.Snapshot {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := archive.Read(file, identities...)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
