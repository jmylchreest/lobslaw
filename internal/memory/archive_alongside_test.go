package memory

import (
	"bytes"
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestAlongsideImportTracksRecordsAcrossGenerations(t *testing.T) {
	node, fsm := newTestRaft(t)
	store := fsm.Store()
	ctx := context.Background()
	original := []archive.Record{archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 1, NextSeq: 2})}
	if _, err := ApplyArchiveImport(ctx, node, store, original, ArchiveImportOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	source := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 10, NextSeq: 12}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 10), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 10, Content: "local history"}),
	}
	opts := ArchiveImportOptions{SourceID: "local-stack", Alongside: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}}
	first, err := ApplyArchiveImport(ctx, node, store, source, opts, nil)
	if err != nil || first.Applied != 2 {
		t.Fatalf("first import: %+v, %v", first, err)
	}
	repeated, err := ApplyArchiveImport(ctx, node, store, source, opts, nil)
	if err != nil || repeated.Applied != 0 || repeated.Completed != 2 {
		t.Fatalf("repeat: %+v, %v", repeated, err)
	}
	// A new record changes the archive content and its retry ID, not the source identity.
	source = append(source, archiveTestRecord(t, "episodic", "new", &lobslawv1.EpisodicRecord{Id: "new"}))
	next, err := ApplyArchiveImport(ctx, node, store, source, opts, nil)
	if err != nil || next.Applied != 1 {
		t.Fatalf("new generation: %+v, %v", next, err)
	}
	existing, err := store.ArchiveRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mapped string
	for _, r := range existing {
		if r.Kind == "sessions" && r.ID != "chat" {
			mapped = r.ID
		}
	}
	if mapped == "" {
		t.Fatal("missing alongside session")
	}
	// The mapping remains after deletion and prevents silent recreation, including same-archive retries.
	if err := store.Delete(BucketSessions, mapped); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanArchiveImport(mustArchiveRecords(t, store), source, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) == 0 {
		t.Fatal("deleted destination was not reported")
	}
	result, err := ApplyArchiveImport(ctx, node, store, source, opts, nil)
	if err == nil || result.Applied != 0 {
		t.Fatalf("deleted destination silently recreated: %+v, %v", result, err)
	}
}

func mustArchiveRecords(t *testing.T, store *Store) []archive.Record {
	t.Helper()
	records, err := store.ArchiveRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestAlongsideRequiresStableSource(t *testing.T) {
	_, err := PlanArchiveImport(nil, nil, ArchiveImportOptions{Alongside: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}})
	if err == nil {
		t.Fatal("alongside accepted without stable source identity")
	}
}

func TestAlongsideProvenanceSurvivesBackupRestore(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	source := []archive.Record{archiveTestRecord(t, "documents", "memory", &lobslawv1.VectorRecord{Id: "memory", Text: "remember"})}
	opts := ArchiveImportOptions{SourceID: "local-stack", Alongside: []ArchiveRecordRef{{Kind: "documents", ID: "memory"}}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), source, opts, stubEmbedder{model: "first"}); err != nil {
		t.Fatal(err)
	}
	snapshot := archive.Snapshot{
		Manifest: archive.Manifest{SnapshotID: "backup", CreatedAt: time.Now().UTC(), SourceID: "destination"},
		Records:  mustArchiveRecords(t, fsm.Store()),
	}
	var encoded bytes.Buffer
	if err := archive.Write(&encoded, snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := archive.Read(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	target, targetFSM := newTestRaft(t)
	if _, err := ApplyArchiveImport(ctx, target, targetFSM.Store(), restored.Records, ArchiveImportOptions{}, stubEmbedder{model: "rebuilt"}); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyArchiveImport(ctx, target, targetFSM.Store(), source, opts, nil)
	if err != nil || result.Applied != 0 || result.Completed != 1 {
		t.Fatalf("restore lost import identity: %+v, %v", result, err)
	}
	changed := []archive.Record{archiveTestRecord(t, "documents", "memory", &lobslawv1.VectorRecord{Id: "memory", Text: "source changed"})}
	plan, err := PlanArchiveImport(mustArchiveRecords(t, targetFSM.Store()), changed, opts)
	if err != nil || len(plan.ConflictDetails) != 1 || plan.ConflictDetails[0].Reason != "source or import options changed" {
		t.Fatalf("changed source: %+v, %v", plan, err)
	}
	// Omitting alongside on a subsequent invocation still finds the saved copy.
	opts.Alongside = nil
	result, err = ApplyArchiveImport(ctx, target, targetFSM.Store(), source, opts, nil)
	if err != nil || result.Applied != 0 {
		t.Fatalf("mapping depended on selection flags: %+v, %v", result, err)
	}
}

func TestDeletedAlongsideMappingSurvivesRestore(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	source := []archive.Record{archiveTestRecord(t, "episodic", "memory", &lobslawv1.EpisodicRecord{Id: "memory"})}
	opts := ArchiveImportOptions{SourceID: "local", Alongside: []ArchiveRecordRef{{Kind: "episodic", ID: "memory"}}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), source, opts, nil); err != nil {
		t.Fatal(err)
	}
	id := "import:" + archiveMappingID(opts.SourceID, "episodic", "memory")
	if err := fsm.Store().Delete(BucketEpisodicRecords, id); err != nil {
		t.Fatal(err)
	}
	backup := mustArchiveRecords(t, fsm.Store())
	target, targetFSM := newTestRaft(t)
	if _, err := ApplyArchiveImport(ctx, target, targetFSM.Store(), backup, ArchiveImportOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanArchiveImport(mustArchiveRecords(t, targetFSM.Store()), source, opts)
	if err != nil || len(plan.ConflictDetails) != 1 || plan.ConflictDetails[0].Reason != "destination deleted" {
		t.Fatalf("restored deletion mapping lost: %+v, %v", plan, err)
	}
}

func TestAlongsideDestinationEditIsNotHiddenByReceipt(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	source := []archive.Record{archiveTestRecord(t, "documents", "memory", &lobslawv1.VectorRecord{Id: "memory", Text: "remember"})}
	opts := ArchiveImportOptions{SourceID: "local-stack", Alongside: []ArchiveRecordRef{{Kind: "documents", ID: "memory"}}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), source, opts, stubEmbedder{model: "test"}); err != nil {
		t.Fatal(err)
	}
	id := "import:" + archiveMappingID(opts.SourceID, "documents", "memory")
	raw, err := proto.Marshal(&lobslawv1.VectorRecord{Id: id, Text: "destination edited"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Store().Put(BucketVectorRecords, id, raw); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanArchiveImport(mustArchiveRecords(t, fsm.Store()), source, opts)
	if err != nil || len(plan.ConflictDetails) != 1 || plan.ConflictDetails[0].Reason != "destination changed" {
		t.Fatalf("destination edit: %+v, %v", plan, err)
	}
	result, err := ApplyArchiveImport(ctx, node, fsm.Store(), source, opts, nil)
	if err == nil || result.Applied != 0 {
		t.Fatalf("receipt hid destination edit: %+v, %v", result, err)
	}
}

func TestTrackedImportAdoptsIdenticalActiveSchedule(t *testing.T) {
	node, fsm := newTestRaft(t)
	task := &lobslawv1.ScheduledTaskRecord{Id: "task", Schedule: "0 9 * * *", Enabled: true}
	raw, err := proto.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Store().Put(BucketScheduledTasks, task.Id, raw); err != nil {
		t.Fatal(err)
	}
	source := []archive.Record{archiveTestRecord(t, "scheduled-tasks", task.Id, task)}
	opts := ArchiveImportOptions{SourceID: "local", SourceTimezone: "UTC"}
	for range 2 {
		result, err := ApplyArchiveImport(context.Background(), node, fsm.Store(), source, opts, nil)
		if err != nil || result.Applied != 0 || result.Completed != 1 {
			t.Fatalf("adoption: %+v, %v", result, err)
		}
	}
}

func TestTrackedMappingRejectsConcurrentDestinationEdit(t *testing.T) {
	node, fsm := newTestRaft(t)
	rec := &lobslawv1.ScheduledTaskRecord{Id: "task", Schedule: "0 9 * * *"}
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Store().Put(BucketScheduledTasks, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
	source := []archive.Record{archiveTestRecord(t, "scheduled-tasks", rec.Id, rec)}
	opts := ArchiveImportOptions{SourceID: "local", SourceTimezone: "UTC"}
	applier := &archiveEditBeforeApply{raft: node, edit: func() {
		rec.Schedule = "0 10 * * *"
		raw, err := proto.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		if err := fsm.Store().Put(BucketScheduledTasks, rec.Id, raw); err != nil {
			t.Fatal(err)
		}
	}}
	result, err := ApplyArchiveImport(context.Background(), applier, fsm.Store(), source, opts, nil)
	if err == nil || result.Applied != 0 {
		t.Fatalf("concurrent edit accepted: %+v, %v", result, err)
	}
	for _, r := range mustArchiveRecords(t, fsm.Store()) {
		if r.Kind == "import-mappings" {
			t.Fatal("mapping survived failed transaction")
		}
	}
}

type archiveEditBeforeApply struct {
	raft RebindApplier
	edit func()
}

func (r *archiveEditBeforeApply) Apply(data []byte, timeout time.Duration) (any, error) {
	r.edit()
	return r.raft.Apply(data, timeout)
}

func TestSkipSessionSkipsItsWholeTranscript(t *testing.T) {
	existing := []archive.Record{archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 1, NextSeq: 2})}
	source := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 10, NextSeq: 12}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 10), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 10}),
	}
	plan, err := PlanArchiveImport(existing, source, ArchiveImportOptions{Skip: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}})
	if err != nil || len(plan.Skipped) != 2 || len(plan.Additions) != 0 || len(plan.Conflicts) != 0 {
		t.Fatalf("session skip: %+v, %v", plan, err)
	}
}
