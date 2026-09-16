package memory

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestArchiveReplacementRequiresBackupAndReplacesWholeSession(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	old := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 1, NextSeq: 2}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 1), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 1, Content: "destination"}),
	}
	incoming := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 10, NextSeq: 11}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 10), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 10, Content: "source"}),
	}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), old, ArchiveImportOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	opts := ArchiveImportOptions{SourceID: "local", Replace: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil); err == nil {
		t.Fatal("replacement accepted without backup")
	}
	var err error
	opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil)
	if err != nil || result.Applied != 2 {
		t.Fatalf("replace: %+v %v", result, err)
	}
	if _, err := fsm.Store().Get(BucketSessionMessages, sessionMessageKey("chat", 1)); err == nil {
		t.Fatal("old transcript survived replacement")
	}
	opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil)
	if err != nil || repeated.Applied != 0 {
		t.Fatalf("replacement retry: %+v %v", repeated, err)
	}
}

func TestArchiveReplacementRejectsConcurrentTranscriptAppend(t *testing.T) {
	node, fsm := newTestRaft(t)
	old := []archive.Record{archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", NextSeq: 2})}
	incoming := []archive.Record{archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", NextSeq: 3})}
	ctx := context.Background()
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), old, ArchiveImportOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	digest, err := ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	opts := ArchiveImportOptions{SourceID: "local", BackupDigest: digest, Replace: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}}
	applier := &archiveEditBeforeApply{raft: node, edit: func() {
		raw, err := proto.Marshal(&lobslawv1.SessionMessage{SessionId: "chat", Seq: 1, Content: "concurrent"})
		if err != nil {
			t.Fatal(err)
		}
		if err := fsm.Store().Put(BucketSessionMessages, sessionMessageKey("chat", 1), raw); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := ApplyArchiveImport(ctx, applier, fsm.Store(), incoming, opts, nil); err == nil {
		t.Fatal("concurrent append accepted")
	}
	raw, err := fsm.Store().Get(BucketSessions, "chat")
	if err != nil {
		t.Fatal(err)
	}
	var session lobslawv1.SessionRecord
	if err := proto.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	if session.NextSeq != 2 {
		t.Fatal("replacement partially applied")
	}
}

func TestArchiveReplacementKeepsMappedDestination(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	record := func(text string) []archive.Record {
		return []archive.Record{archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", UserId: text})}
	}
	opts := ArchiveImportOptions{SourceID: "local", Alongside: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}, Owners: map[string]string{"first": "first", "second": "second"}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), record("first"), opts, nil); err != nil {
		t.Fatal(err)
	}
	opts.Alongside = nil
	opts.Replace = []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}
	var err error
	opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyArchiveImport(ctx, node, fsm.Store(), record("second"), opts, nil)
	if err != nil || result.Applied != 1 {
		t.Fatalf("replace mapped: %+v %v", result, err)
	}
	if _, err := fsm.Store().Get(BucketSessions, "chat"); err == nil {
		t.Fatal("created an unmapped duplicate")
	}
	target := "import:" + archiveMappingID("local", "sessions", "chat")
	if _, err := fsm.Store().Get(BucketSessions, target); err != nil {
		t.Fatal(err)
	}
	opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	result, err = ApplyArchiveImport(ctx, node, fsm.Store(), record("second"), opts, nil)
	if err != nil || result.Applied != 0 {
		t.Fatalf("repeat: %+v %v", result, err)
	}
}

func TestArchiveReplacementDoesNotReuseAnEarlierReceipt(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	old := []archive.Record{archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", NextSeq: 2})}
	incoming := []archive.Record{archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", NextSeq: 3})}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), old, ArchiveImportOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	digest, err := ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	opts := ArchiveImportOptions{SourceID: "local", BackupDigest: digest, Replace: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil); err != nil {
		t.Fatal(err)
	}
	// A subsequent recovery can restore the exact old portable state while
	// retaining the transaction journal. A new replacement must still execute.
	raw, err := proto.Marshal(&lobslawv1.SessionRecord{Id: "chat", NextSeq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Store().Put(BucketSessions, "chat", raw); err != nil {
		t.Fatal(err)
	}
	if err := fsm.Store().Delete(BucketArchiveMappings, archiveMappingID("local", "sessions", "chat")); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil); err != nil {
		t.Fatal(err)
	}
	raw, err = fsm.Store().Get(BucketSessions, "chat")
	if err != nil {
		t.Fatal(err)
	}
	var actual lobslawv1.SessionRecord
	if err := proto.Unmarshal(raw, &actual); err != nil {
		t.Fatal(err)
	}
	if actual.NextSeq != 3 {
		t.Fatal("old receipt hid the new replacement")
	}
}
