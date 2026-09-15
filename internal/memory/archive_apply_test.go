package memory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type interruptedArchiveRaft struct {
	raft      RebindApplier
	remaining int
}

func TestArchiveImportResumesAfterRaftAndStoreRestart(t *testing.T) {
	for _, source := range []string{"", "stable-source"} {
		t.Run(source, func(t *testing.T) { testArchiveImportRestart(t, ArchiveImportOptions{SourceID: source}) })
	}
}

func testArchiveImportRestart(t *testing.T, opts ArchiveImportOptions) {
	t.Helper()
	node, fsm := newTestRaft(t)
	store := fsm.Store()
	path := store.loadDB().Path()
	key := store.key
	records := []archive.Record{
		archiveTestRecord(t, "documents", "a", &lobslawv1.VectorRecord{Id: "a", Text: "first"}),
		archiveTestRecord(t, "documents", "b", &lobslawv1.VectorRecord{Id: "b", Text: "second"}),
	}
	ctx := context.Background()
	first, err := ApplyArchiveImport(ctx, &interruptedArchiveRaft{raft: node, remaining: 1}, store, records, opts, stubEmbedder{model: "destination"})
	if err == nil || first.Applied != 1 {
		t.Fatalf("expected partial import: %+v, %v", first, err)
	}
	if err := node.Shutdown(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	_, transport := raft.NewInmemTransport("test-node")
	restarted, err := NewRaft(RaftConfig{
		NodeID: "test-node", LocalAddr: "test-node",
		DataDir: filepath.Dir(path), Transport: transport,
	}, NewFSM(reopened))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Shutdown() })
	if err := restarted.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyArchiveImport(ctx, restarted, reopened, records, opts, stubEmbedder{model: "destination"})
	if err != nil || result.Applied != 1 || result.Completed != 2 || result.ImportID != first.ImportID {
		t.Fatalf("restart lost resume state: %+v, %v", result, err)
	}
}

func (r *interruptedArchiveRaft) Apply(data []byte, timeout time.Duration) (any, error) {
	if r.remaining == 0 {
		return nil, errors.New("simulated disconnect")
	}
	r.remaining--
	return r.raft.Apply(data, timeout)
}

func TestArchiveApplyResumesWithoutOverwritingDestinationEdits(t *testing.T) {
	node, fsm := newTestRaft(t)
	source := []archive.Record{
		archiveTestRecord(t, "documents", "a", &lobslawv1.VectorRecord{Id: "a", Text: "original summary"}),
		archiveTestRecord(t, "documents", "b", &lobslawv1.VectorRecord{Id: "b", Text: "standalone document"}),
	}
	ctx := context.Background()
	interrupted := &interruptedArchiveRaft{raft: node, remaining: 1}
	first, err := ApplyArchiveImport(ctx, interrupted, fsm.Store(), source, ArchiveImportOptions{}, stubEmbedder{model: "destination"})
	if err == nil || first.Applied != 1 {
		t.Fatalf("expected honest partial progress: %+v, %v", first, err)
	}
	raw, err := fsm.Store().Get(BucketVectorRecords, "a")
	if err != nil {
		t.Fatal(err)
	}
	var rec lobslawv1.VectorRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.EmbeddingModel != "destination" || len(rec.Embedding) != 4 || rec.Norm == 0 {
		t.Fatal("destination embedding was not rebuilt")
	}
	rec.Text = "edited after partial import"
	entry := putEntry("a", &lobslawv1.LogEntry{
		Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &rec},
	})
	data, err := proto.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Apply(data, time.Second); err != nil {
		t.Fatal(err)
	}
	resumed, err := ApplyArchiveImport(ctx, node, fsm.Store(), source, ArchiveImportOptions{}, stubEmbedder{model: "destination"})
	if err != nil || resumed.Applied != 1 || resumed.Completed != 2 || resumed.ImportID != first.ImportID {
		t.Fatalf("resume failed: %+v, %v", resumed, err)
	}
	raw, err = fsm.Store().Get(BucketVectorRecords, "a")
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Text != "edited after partial import" {
		t.Fatal("resume overwrote a destination edit")
	}
	repeated, err := ApplyArchiveImport(ctx, node, fsm.Store(), source, ArchiveImportOptions{}, nil)
	if err != nil || repeated.Applied != 0 || repeated.Completed != 2 {
		t.Fatalf("completed import was not a no-op: %+v, %v", repeated, err)
	}
}

func TestArchiveApplyConflictWritesNothing(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	existing := []archive.Record{archiveTestRecord(t, "documents", "b", &lobslawv1.VectorRecord{Id: "b", Text: "existing"})}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), existing, ArchiveImportOptions{}, stubEmbedder{model: "destination"}); err != nil {
		t.Fatal(err)
	}
	source := []archive.Record{
		archiveTestRecord(t, "documents", "a", &lobslawv1.VectorRecord{Id: "a", Text: "new"}),
		archiveTestRecord(t, "documents", "b", &lobslawv1.VectorRecord{Id: "b", Text: "conflict"}),
	}
	result, err := ApplyArchiveImport(ctx, node, fsm.Store(), source, ArchiveImportOptions{}, stubEmbedder{model: "destination"})
	if err == nil || result.Applied != 0 {
		t.Fatalf("conflict allowed writes: %+v, %v", result, err)
	}
	if _, err := fsm.Store().Get(BucketVectorRecords, "a"); err == nil {
		t.Fatal("partial write before conflict")
	}
}

func TestArchiveImportRefusesToMixDestinationEmbeddingModels(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	first := []archive.Record{archiveTestRecord(t, "documents", "a", &lobslawv1.VectorRecord{Id: "a", Text: "old corpus"})}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), first, ArchiveImportOptions{}, stubEmbedder{model: "old"}); err != nil {
		t.Fatal(err)
	}
	next := []archive.Record{archiveTestRecord(t, "documents", "b", &lobslawv1.VectorRecord{Id: "b", Text: "new corpus"})}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), next, ArchiveImportOptions{}, stubEmbedder{model: "new"}); err == nil {
		t.Fatal("mixed embedding models in the destination")
	}
}

func TestArchiveBatchRollsBackRecordsAndReceiptOnConflict(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	records := []archive.Record{
		archiveTestRecord(t, "documents", "a", &lobslawv1.VectorRecord{Id: "a", Text: "first"}),
		archiveTestRecord(t, "documents", "b", &lobslawv1.VectorRecord{Id: "b", Text: "second"}),
	}
	sources, err := archiveMappedSources(records, ArchiveImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := prepareArchiveBatch(ctx, fsm.Store(), "batch-test", records, sources, stubEmbedder{model: "destination"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := proto.Marshal(&lobslawv1.VectorRecord{Id: "b", Text: "concurrent write"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Store().Put(BucketVectorRecords, "b", raw); err != nil {
		t.Fatal(err)
	}
	entry := putEntry(batch.BatchId, &lobslawv1.LogEntry{
		Payload: &lobslawv1.LogEntry_ArchiveBatch{ArchiveBatch: batch},
	})
	data, err := proto.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	response, err := node.Apply(data, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if applyErr, ok := response.(error); !ok || applyErr == nil {
		t.Fatal("concurrent write did not invalidate batch")
	}
	if _, err := fsm.Store().Get(BucketVectorRecords, "a"); err == nil {
		t.Fatal("failed batch left a partial record")
	}
	if _, err := fsm.Store().Get(BucketArchiveImports, "batch-test/"+batch.BatchId); err == nil {
		t.Fatal("failed batch left a completion receipt")
	}
}
