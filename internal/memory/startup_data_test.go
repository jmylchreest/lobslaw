package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func startupLegacyData(t *testing.T) (string, crypto.Key) {
	t.Helper()
	dir, key := legacyDataDirectory(t)
	logs, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, RaftLogFile)})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := dataformat.NormalizeLog(mustReadStartupFile(t, "../dataformat/testdata/main-v0.binpb"), dataformat.LegacyMain)
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.StoreLog(&raft.Log{Index: 1, Term: 7, Type: raft.LogCommand, Data: entry}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, key
}

func mustReadStartupFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestStartupMigratesSupportedStateAndRetainsNodeBackup(t *testing.T) {
	dir, key := startupLegacyData(t)
	beforeState := mustReadStartupFile(t, filepath.Join(dir, "state.db"))
	beforeRaft := mustReadStartupFile(t, filepath.Join(dir, RaftLogFile))
	store, report, err := OpenNodeStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Migrated || report.Before.Version != 0 || report.After.Version != 1 || report.Backup == "" {
		t.Fatalf("unexpected startup report: %+v", report)
	}
	raw, err := store.Get(BucketPinned, "user-memory")
	if err != nil {
		t.Fatal(err)
	}
	var memory pb.PinnedMemory
	if err := proto.Unmarshal(raw, &memory); err != nil {
		t.Fatal(err)
	}
	if memory.GetId() != "user-memory" || len(memory.Entries) != 1 || memory.Entries[0] != "keep me" {
		t.Fatal("memories changed during startup migration")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeState, mustReadStartupFile(t, filepath.Join(report.Backup, "state.db"))) {
		t.Fatal("backup changed original state bytes")
	}
	if !bytes.Equal(beforeRaft, mustReadStartupFile(t, filepath.Join(report.Backup, RaftLogFile))) {
		t.Fatal("backup changed Raft bytes")
	}
	if !bytes.Equal(beforeRaft, mustReadStartupFile(t, filepath.Join(dir, RaftLogFile))) {
		t.Fatal("migration rewrote the live Raft log")
	}
	info, err := os.Stat(report.Backup)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatal("backup directory is not private")
	}
	info, err = os.Stat(filepath.Join(report.Backup, "state.db"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("backup state is not private")
	}
	if store, _, err := OpenNodeStore(report.Backup, key); err == nil {
		_ = store.Close()
		t.Fatal("backup started as a live node")
	}
	store, again, err := OpenNodeStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if again.Migrated || again.Backup != "" || again.Before != again.After {
		t.Fatal("restart repeated the migration")
	}
}

func TestStartupRefusesAmbiguousLogsBeforeChangingLegacyState(t *testing.T) {
	dir, key := legacyDataDirectory(t)
	state := mustReadStartupFile(t, filepath.Join(dir, "state.db"))
	logs := mustReadStartupFile(t, filepath.Join(dir, RaftLogFile))
	if store, _, err := OpenNodeStore(dir, key); err == nil {
		_ = store.Close()
		t.Fatal("startup guessed legacy provenance")
	}
	if !bytes.Equal(state, mustReadStartupFile(t, filepath.Join(dir, "state.db"))) {
		t.Fatal("refused startup migrated state first")
	}
	if !bytes.Equal(logs, mustReadStartupFile(t, filepath.Join(dir, RaftLogFile))) {
		t.Fatal("refused startup modified logs")
	}
	backups, err := filepath.Glob(filepath.Join(dir, "startup-backup-*"))
	if err != nil || len(backups) != 0 {
		t.Fatal("refused input published a backup")
	}
}

func TestStartupRejectsUnsupportedLogsWrongKeysAndSnapshotsBeforeMigration(t *testing.T) {
	for _, scenario := range []string{"future-log", "wrong-key", "invalid-snapshot-directory"} {
		t.Run(scenario, func(t *testing.T) {
			dir, key := startupLegacyData(t)
			switch scenario {
			case "future-log":
				logs, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, RaftLogFile)})
				if err != nil {
					t.Fatal(err)
				}
				entry, err := proto.Marshal(&pb.LogEntry{SchemaVersion: 999, Op: pb.LogOp_LOG_OP_DELETE, Id: "future"})
				if err != nil {
					t.Fatal(err)
				}
				if err := logs.StoreLog(&raft.Log{Index: 1, Term: 7, Type: raft.LogCommand, Data: entry}); err != nil {
					t.Fatal(err)
				}
				_ = logs.Close()
			case "wrong-key":
				key[0]++
			case "invalid-snapshot-directory":
				if err := os.WriteFile(filepath.Join(dir, SnapshotDir), []byte("not a repository"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			state := mustReadStartupFile(t, filepath.Join(dir, "state.db"))
			if store, _, err := OpenNodeStore(dir, key); err == nil {
				_ = store.Close()
				t.Fatal("unsupported input was opened")
			}
			if !bytes.Equal(state, mustReadStartupFile(t, filepath.Join(dir, "state.db"))) {
				t.Fatal("unsupported input changed state")
			}
		})
	}
}

func TestStartupInitializesEmptyDataAndDefersContractActivation(t *testing.T) {
	var key crypto.Key
	key[0] = 42
	dir := filepath.Join(t.TempDir(), "new-node")
	store, report, err := OpenNodeStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if report.Migrated || report.Backup != "" || report.After.Version != 1 {
		t.Fatalf("bad fresh-node report: %+v", report)
	}
	if err := store.loadDB().View(func(tx *bolt.Tx) error {
		for _, name := range teamBuckets {
			if tx.Bucket([]byte(name)) != nil {
				t.Error("new container activated a team bucket without the cluster transition")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
}

func TestStartupUnsupportedStateKeepsBytes(t *testing.T) {
	dir, key := startupLegacyData(t)
	db, err := bolt.Open(filepath.Join(dir, "state.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte(formatBucket))
		if err != nil {
			return err
		}
		raw, err := json.Marshal(StateFormat{Version: 999, Protocol: "future"})
		if err != nil {
			return err
		}
		return b.Put([]byte(formatKey), raw)
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	state := mustReadStartupFile(t, filepath.Join(dir, "state.db"))
	if store, _, err := OpenNodeStore(dir, key); err == nil {
		_ = store.Close()
		t.Fatal("future state accepted")
	}
	if !bytes.Equal(state, mustReadStartupFile(t, filepath.Join(dir, "state.db"))) {
		t.Fatal("future state modified")
	}
}

func TestStartupBackupCanBeInspectedForExplicitRecovery(t *testing.T) {
	dir, key := startupLegacyData(t)
	store, report, err := OpenNodeStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	inspection, err := InspectData(context.Background(), report.Backup, "", key)
	if err != nil || inspection.State.Version != 0 || inspection.LastLog != 1 {
		t.Fatalf("backup not inspectable: %+v, %v", inspection, err)
	}
}

func TestStartupRetainsAndValidatesHistoricalSnapshots(t *testing.T) {
	dir, key := startupLegacyData(t)
	state := mustReadStartupFile(t, filepath.Join(dir, "state.db"))
	snapshots, err := raft.NewFileSnapshotStore(filepath.Join(dir, SnapshotDir), 2, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := raft.NewInmemTransport("fixture-node")
	sink, err := snapshots.Create(raft.SnapshotVersionMax, 1, 7, raft.Configuration{Servers: []raft.Server{{ID: "fixture-node", Address: "fixture-node", Suffrage: raft.Voter}}}, 1, transport)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(state); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	store, report, err := OpenNodeStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	copied, err := raft.NewFileSnapshotStore(filepath.Join(report.Backup, SnapshotDir), 2, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, reader, err := copied.Open(sink.ID())
	if err != nil {
		t.Fatal(err)
	}
	image, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(state, image) {
		t.Fatal("backup lost or modified the historical snapshot", err)
	}
	if err := validatePhysicalSnapshot(bytes.NewReader(image), key); err != nil {
		t.Fatal(err)
	}
}

func TestStartupPreflightsRetainedLogsBeforeCreatingMissingState(t *testing.T) {
	dir, key := legacyDataDirectory(t)
	if err := os.Remove(filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
	if store, _, err := OpenNodeStore(dir, key); err == nil {
		_ = store.Close()
		t.Fatal("ambiguous retained logs accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.db")); !os.IsNotExist(err) {
		t.Fatal("refused startup created an empty state database", err)
	}
}

func TestStartupRefusesIncompletePopulatedLegacyData(t *testing.T) {
	dir, key := startupLegacyData(t)
	if err := os.Remove(filepath.Join(dir, RaftLogFile)); err != nil {
		t.Fatal(err)
	}
	state := mustReadStartupFile(t, filepath.Join(dir, "state.db"))
	if store, _, err := OpenNodeStore(dir, key); err == nil {
		_ = store.Close()
		t.Fatal("incomplete legacy node data migrated")
	}
	if !bytes.Equal(state, mustReadStartupFile(t, filepath.Join(dir, "state.db"))) {
		t.Fatal("incomplete data changed before refusal")
	}
}

func TestStartupBackupFailurePreventsMigration(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("filesystem permission refusal requires an unprivileged user")
	}
	dir, key := startupLegacyData(t)
	state := mustReadStartupFile(t, filepath.Join(dir, "state.db"))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if store, _, err := OpenNodeStore(dir, key); err == nil {
		_ = store.Close()
		t.Fatal("migrated without a durable backup")
	}
	if !bytes.Equal(state, mustReadStartupFile(t, filepath.Join(dir, "state.db"))) {
		t.Fatal("backup failure changed source state")
	}
}

func TestStartupPreservesAuxiliaryFilesAndIgnoresStaleStaging(t *testing.T) {
	dir, key := startupLegacyData(t)
	files := map[string][]byte{"auth/browser-sessions.json": []byte("existing sessions"), "operator-ca.key": []byte("existing identity"), ".startup-backup-interrupted/partial": []byte("incomplete")}
	for path, data := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store, report, err := OpenNodeStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	if !report.Migrated {
		t.Fatal("stale staging blocked supported migration")
	}
	for path, data := range files {
		if !bytes.Equal(data, mustReadStartupFile(t, filepath.Join(dir, path))) {
			t.Fatal("startup changed an auxiliary file", path)
		}
	}
}
