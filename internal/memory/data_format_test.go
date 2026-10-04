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

func legacyDataDirectory(t *testing.T) (string, crypto.Key) {
	t.Helper()
	dir := t.TempDir()
	var key crypto.Key
	key[0] = 42
	cipher, err := crypto.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(filepath.Join(dir, "state.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(&pb.PinnedMemory{Id: "user-memory", Entries: []string{"keep me"}})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := cipher.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucket([]byte(BucketPinned))
		if err != nil {
			return err
		}
		return b.Put([]byte("user-memory"), sealed)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err := raftboltdb.New(raftboltdb.Options{Path: filepath.Join(dir, RaftLogFile)})
	if err != nil {
		t.Fatal(err)
	}
	historical, err := os.ReadFile("../dataformat/testdata/main-v0.binpb")
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.StoreLog(&raft.Log{Index: 1, Term: 7, Type: raft.LogCommand, Data: historical}); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, key
}

func TestPhysicalMigrationPreservesSourceAndRequiresAcknowledgement(t *testing.T) {
	source, key := legacyDataDirectory(t)
	original, err := os.ReadFile(filepath.Join(source, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := InspectData(context.Background(), source, "", key); err == nil {
		t.Fatal("guessed ambiguous source")
	}
	dest := filepath.Join(t.TempDir(), "converted")
	report, err := MigrateData(context.Background(), source, dest, dataformat.LegacyMain, key)
	if err != nil {
		t.Fatal(err)
	}
	if !report.RestoreRequired || report.LastLog != 1 {
		t.Fatalf("bad report %+v", report)
	}
	after, err := os.ReadFile(filepath.Join(source, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, after) {
		t.Fatal("changed source database")
	}
	beforeLog, err := os.ReadFile(filepath.Join(source, RaftLogFile))
	if err != nil {
		t.Fatal(err)
	}
	afterLog, err := os.ReadFile(filepath.Join(dest, RaftLogFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeLog, afterLog) {
		t.Fatal("rewrote committed log")
	}
	store, err := OpenStore(filepath.Join(dest, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := store.Get(BucketPinned, "user-memory"); err != nil || len(raw) == 0 {
		t.Fatal("lost record", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := AcceptDataRecovery(context.Background(), dest, key); err != nil {
		t.Fatal(err)
	}
	manifest, err := dataformat.ReadManifest(dest)
	if err != nil || manifest.RestoreRequired {
		t.Fatal("acknowledgement not durable", err)
	}
	if _, err := MigrateData(context.Background(), source, dest, dataformat.LegacyMain, key); err == nil {
		t.Fatal("overwrote destination")
	}
}

func TestMigrationRejectsWrongKeyCancellationAndSymlinks(t *testing.T) {
	source, key := legacyDataDirectory(t)
	wrong := key
	wrong[0]++
	if _, err := InspectData(context.Background(), source, dataformat.LegacyMain, wrong); err == nil {
		t.Fatal("accepted wrong key")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := filepath.Join(t.TempDir(), "cancelled")
	if _, err := MigrateData(ctx, source, dest, dataformat.LegacyMain, key); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("published cancelled migration")
	}
	if err := os.Symlink(source, filepath.Join(t.TempDir(), "source-link")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(source, "snapshots")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectData(context.Background(), source, dataformat.LegacyMain, key); err == nil {
		t.Fatal("followed snapshot symlink")
	}
}

func TestSnapshotFutureVersionPreservesLiveState(t *testing.T) {
	_, key := legacyDataDirectory(t)
	source, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	if err := source.loadDB().Update(func(tx *bolt.Tx) error {
		raw, _ := json.Marshal(StateFormat{Version: 999, Protocol: dataformat.ClusterProtocol})
		return tx.Bucket([]byte(formatBucket)).Put([]byte(formatKey), raw)
	}); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := source.WriteSnapshot(&snapshot); err != nil {
		t.Fatal(err)
	}
	target, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	if err := target.Put(BucketPinned, "kept", []byte("original")); err != nil {
		t.Fatal(err)
	}
	if err := target.RestoreFromSnapshot(&snapshot); err == nil {
		t.Fatal("accepted future snapshot")
	}
	got, err := target.Get(BucketPinned, "kept")
	if err != nil || string(got) != "original" {
		t.Fatal("destroyed live data", err)
	}
}

func TestHistoricalSnapshotMigrationAndRestart(t *testing.T) {
	source, key := legacyDataDirectory(t)
	snapshots, err := raft.NewFileSnapshotStore(filepath.Join(source, SnapshotDir), 2, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := raft.NewInmemTransport("fixture-node")
	sink, err := snapshots.Create(raft.SnapshotVersionMax, 1, 7, raft.Configuration{Servers: []raft.Server{{ID: "fixture-node", Address: "fixture-node", Suffrage: raft.Voter}}}, 1, transport)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(source, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "recovered")
	report, err := MigrateData(context.Background(), source, dest, dataformat.LegacyMain, key)
	if err != nil {
		t.Fatal(err)
	}
	if report.Snapshots != 1 {
		t.Fatal("snapshot lost", report)
	}
	// Replay/copy does not alter original snapshot bytes or its checksum.
	_, reader, err := snapshots.Open(sink.ID())
	if err != nil {
		t.Fatal(err)
	}
	copied, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(raw, copied) {
		t.Fatal("changed source snapshot", err)
	}
	store, err := OpenStore(filepath.Join(dest, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	// The historical snapshot itself is still readable through the same staged
	// restore path that Raft invokes on restart and InstallSnapshot.
	if err := store.RestoreFromSnapshot(bytes.NewReader(copied)); err != nil {
		t.Fatal(err)
	}
	if raw, err := store.Get(BucketPinned, "user-memory"); err != nil || len(raw) == 0 {
		t.Fatal("lost snapshot state", err)
	}
}
