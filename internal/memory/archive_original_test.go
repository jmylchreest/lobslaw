package memory

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/archive"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestArchiveReplaceOriginalMovesAlongsideSession(t *testing.T) {
	node, fsm := newTestRaft(t)
	ctx := context.Background()
	records := func(seq uint64, text string) []archive.Record {
		return []archive.Record{
			archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: seq, NextSeq: seq + 1}),
			archiveTestRecord(t, "session-messages", sessionMessageKey("chat", seq), &lobslawv1.SessionMessage{SessionId: "chat", Seq: seq, Content: text}),
		}
	}
	original, incoming := records(1, "cluster"), records(10, "local")
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), original, ArchiveImportOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	opts := ArchiveImportOptions{SourceID: "local", Alongside: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil); err != nil {
		t.Fatal(err)
	}
	opts.Alongside = nil
	opts.ReplaceOriginal = []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}}
	if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil); err == nil {
		t.Fatal("replacement without backup")
	}
	var err error
	opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	result, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil)
	if err != nil || result.Applied != 2 {
		t.Fatalf("replace original: %+v %v", result, err)
	}
	oldCopy := "import:" + archiveMappingID("local", "sessions", "chat")
	if _, err := fsm.Store().Get(BucketSessions, oldCopy); err == nil {
		t.Fatal("redundant alongside session retained")
	}
	for _, id := range []string{sessionMessageKey("chat", 1), sessionMessageKey(oldCopy, 10)} {
		if _, err := fsm.Store().Get(BucketSessionMessages, id); err == nil {
			t.Fatal("old transcript retained")
		}
	}
	raw, err := fsm.Store().Get(BucketSessionMessages, sessionMessageKey("chat", 10))
	if err != nil {
		t.Fatal(err)
	}
	var msg lobslawv1.SessionMessage
	if err := proto.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Content != "local" {
		t.Fatal("original does not contain source")
	}
	opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
	if err != nil {
		t.Fatal(err)
	}
	result, err = ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil)
	if err != nil || result.Applied != 0 {
		t.Fatalf("repeat: %+v %v", result, err)
	}
	opts.ReplaceOriginal = nil
	result, err = ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil)
	if err != nil || result.Applied != 0 {
		t.Fatalf("ordinary repeat: %+v %v", result, err)
	}
}

func TestArchiveReplaceOriginalMovesScheduleAndGuardsBothCopies(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		name := "replace"
		if concurrent {
			name = "concurrent edit"
		}
		t.Run(name, func(t *testing.T) {
			node, fsm := newTestRaft(t)
			ctx := context.Background()
			record := func(schedule string) []archive.Record {
				return []archive.Record{archiveTestRecord(t, "scheduled-tasks", "job", &lobslawv1.ScheduledTaskRecord{Id: "job", Schedule: schedule, Enabled: true})}
			}
			opts := ArchiveImportOptions{SourceTimezone: "UTC"}
			if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), record("0 1 * * *"), opts, nil); err != nil {
				t.Fatal(err)
			}
			opts.SourceID = "local"
			opts.Alongside = []ArchiveRecordRef{{Kind: "scheduled-tasks", ID: "job"}}
			incoming := record("0 2 * * *")
			if _, err := ApplyArchiveImport(ctx, node, fsm.Store(), incoming, opts, nil); err != nil {
				t.Fatal(err)
			}
			opts.Alongside = nil
			opts.ReplaceOriginal = []ArchiveRecordRef{{Kind: "scheduled-tasks", ID: "job"}}
			var err error
			opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
			if err != nil {
				t.Fatal(err)
			}
			var applier RebindApplier = node
			target := "import:" + archiveMappingID("local", "scheduled-tasks", "job")
			if concurrent {
				applier = &archiveEditBeforeApply{raft: node, edit: func() {
					raw, err := proto.Marshal(&lobslawv1.ScheduledTaskRecord{Id: target, Schedule: "CRON_TZ=UTC 0 3 * * *"})
					if err != nil {
						t.Fatal(err)
					}
					if err := fsm.Store().Put(BucketScheduledTasks, target, raw); err != nil {
						t.Fatal(err)
					}
				}}
			}
			result, err := ApplyArchiveImport(ctx, applier, fsm.Store(), incoming, opts, nil)
			if concurrent {
				if err == nil {
					t.Fatal("concurrent change accepted")
				}
				return
			}
			if err != nil || result.Applied != 1 {
				t.Fatalf("result: %+v %v", result, err)
			}
			if _, err := fsm.Store().Get(BucketScheduledTasks, target); err == nil {
				t.Fatal("alongside job retained")
			}
			raw, err := fsm.Store().Get(BucketScheduledTasks, "job")
			if err != nil {
				t.Fatal(err)
			}
			var task lobslawv1.ScheduledTaskRecord
			if err := proto.Unmarshal(raw, &task); err != nil {
				t.Fatal(err)
			}
			if task.Enabled || task.Schedule != "CRON_TZ=UTC 0 2 * * *" {
				t.Fatalf("incorrect replaced schedule: enabled=%v cron=%s", task.Enabled, task.Schedule)
			}
		})
	}
}

// Build a saved alongside baseline without depending on an external archive.
func archiveOriginalFixture(t *testing.T) ([]archive.Record, []archive.Record, ArchiveImportOptions) {
	t.Helper()
	incoming := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 1, NextSeq: 3}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 1), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 1, Content: "first source message"}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 2), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 2, Content: "second source message"}),
		archiveTestRecord(t, "scheduled-tasks", "job", &lobslawv1.ScheduledTaskRecord{Id: "job", Schedule: "0 2 * * *"}),
	}
	existing := []archive.Record{
		archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 10, NextSeq: 11}),
		archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 10), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 10, Content: "original message"}),
		archiveTestRecord(t, "scheduled-tasks", "job", &lobslawv1.ScheduledTaskRecord{Id: "job", Schedule: "CRON_TZ=UTC 0 1 * * *"}),
	}
	opts := ArchiveImportOptions{SourceID: "local", SourceTimezone: "UTC", Alongside: []ArchiveRecordRef{{Kind: "sessions", ID: "chat"}, {Kind: "scheduled-tasks", ID: "job"}}}
	plan, err := PlanArchiveImport(existing, incoming, opts)
	if err != nil || len(plan.Conflicts) != 0 {
		t.Fatalf("alongside fixture: %+v %v", plan, err)
	}
	existing = append(existing, plan.Additions...)
	opts.ReplaceOriginal, opts.Alongside = opts.Alongside, nil
	return existing, incoming, opts
}

func TestArchiveReplaceOriginalRejectsDestinationChangesBeforeBackup(t *testing.T) {
	for _, change := range []string{"schedule edit", "schedule deletion", "session edit", "session deletion", "message edit", "message deletion", "message omitted by source", "transcript append", "untracked message"} {
		t.Run(change, func(t *testing.T) {
			existing, incoming, opts := archiveOriginalFixture(t)
			copyID := "import:" + archiveMappingID("local", "sessions", "chat")
			jobID := "import:" + archiveMappingID("local", "scheduled-tasks", "job")
			replace := func(kind, id string, msg proto.Message) {
				existing = slices.DeleteFunc(existing, func(record archive.Record) bool { return record.Kind == kind && record.ID == id })
				if msg != nil {
					existing = append(existing, archiveTestRecord(t, kind, id, msg))
				}
			}
			reason := "destination changed"
			switch change {
			case "schedule edit":
				replace("scheduled-tasks", jobID, &lobslawv1.ScheduledTaskRecord{Id: jobID, Schedule: "CRON_TZ=UTC 0 4 * * *"})
			case "schedule deletion":
				replace("scheduled-tasks", jobID, nil)
				reason = "destination deleted"
			case "session edit":
				replace("sessions", copyID, &lobslawv1.SessionRecord{Id: copyID, FirstSeq: 2, NextSeq: 3})
			case "session deletion":
				replace("sessions", copyID, nil)
				reason = "destination deleted"
			case "message edit", "message omitted by source":
				replace("session-messages", sessionMessageKey(copyID, 2), &lobslawv1.SessionMessage{SessionId: copyID, Seq: 2, Content: "destination edit"})
				if change == "message omitted by source" {
					incoming = slices.DeleteFunc(incoming, func(record archive.Record) bool { return record.ID == sessionMessageKey("chat", 2) })
					incoming[0] = archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 1, NextSeq: 2})
				}
			case "message deletion":
				replace("session-messages", sessionMessageKey(copyID, 2), nil)
				reason = "destination deleted"
			case "transcript append":
				replace("sessions", copyID, &lobslawv1.SessionRecord{Id: copyID, FirstSeq: 1, NextSeq: 4})
				replace("session-messages", sessionMessageKey(copyID, 3), &lobslawv1.SessionMessage{SessionId: copyID, Seq: 3, Content: "continued conversation"})
			case "untracked message":
				// Even loss of a message's baseline must not license deletion.
				replace("import-mappings", archiveMappingID("local", "session-messages", sessionMessageKey("chat", 2)), nil)
				reason = "destination added"
			}
			plan, err := PlanArchiveImport(existing, incoming, opts)
			if err != nil || len(plan.Conflicts) != 1 || len(plan.ConflictDetails) == 0 || !strings.Contains(plan.ConflictDetails[0].Reason, reason) {
				t.Fatalf("expected explicit %s conflict: %+v %v", reason, plan, err)
			}
			if len(plan.Additions) != 0 || len(plan.Removed) != 0 || len(plan.Replaced) != 0 {
				t.Fatal("blocked retirement planned mutations")
			}
			node, fsm := newTestRaft(t)
			seedArchiveOriginalRecords(t, fsm.Store(), existing)
			// Pin the backup AFTER the edit: the state guard alone accepts it.
			opts.BackupDigest, err = ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
			if err != nil {
				t.Fatal(err)
			}
			opts.KeepExisting = true // Must not bypass tracked destination conflicts.
			result, err := ApplyArchiveImport(context.Background(), node, fsm.Store(), incoming, opts, nil)
			if err == nil || !strings.Contains(err.Error(), "conflicts") || result.Applied != 0 {
				t.Fatalf("retirement must refuse edited destination: %+v %v", result, err)
			}
			after, err := ArchiveStateDigest(mustArchiveRecords(t, fsm.Store()))
			if err != nil || after != opts.BackupDigest {
				t.Fatalf("blocked import changed destination: %v", err)
			}
		})
	}
}

func TestArchiveReplaceOriginalPreviewListsBothGroupsWithoutContent(t *testing.T) {
	existing, incoming, opts := archiveOriginalFixture(t)
	plan, err := PlanArchiveImport(existing, incoming, opts)
	if err != nil || len(plan.Conflicts) != 0 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var preview ArchiveImportPlan
	if err := json.Unmarshal(raw, &preview); err != nil {
		t.Fatal(err)
	}
	// Every existing record in this fixture belongs to one of the replaced
	// originals, retired copies, or retargeted mappings.
	if len(preview.Removed) != len(existing) {
		t.Fatalf("missing removals: %s", raw)
	}
	for _, record := range existing {
		if !slices.Contains(preview.Removed, ArchiveRecordRef{Kind: record.Kind, ID: record.ID}) {
			t.Fatalf("missing removal %s/%s", record.Kind, record.ID)
		}
	}
	if strings.Contains(string(raw), "source message") || strings.Contains(string(raw), "original message") || strings.Contains(string(raw), "CRON_TZ") {
		t.Fatalf("content leaked into preview: %s", raw)
	}
}

func TestArchiveReplaceOriginalSelectionGuards(t *testing.T) {
	for _, guard := range []struct{ name, want string }{
		{"source", "stable --source-id"},
		{"kind", "supports sessions and scheduled-tasks only"},
		{"mapping", "requires an existing import mapping"},
	} {
		t.Run(guard.name, func(t *testing.T) {
			existing, incoming, opts := archiveOriginalFixture(t)
			switch guard.name {
			case "source":
				opts.SourceID = ""
			case "kind":
				incoming = append(incoming, archiveTestRecord(t, "documents", "doc", &lobslawv1.VectorRecord{Id: "doc", Text: "source"}))
				opts.ReplaceOriginal = []ArchiveRecordRef{{Kind: "documents", ID: "doc"}}
			case "mapping":
				existing = slices.DeleteFunc(existing, func(record archive.Record) bool { return record.Kind == "import-mappings" })
			}
			_, err := PlanArchiveImport(existing, incoming, opts)
			if err == nil || !strings.Contains(err.Error(), guard.want) {
				t.Fatalf("want %q, got %v", guard.want, err)
			}
		})
	}
}

func seedArchiveOriginalRecords(t *testing.T, store *Store, records []archive.Record) {
	t.Helper()
	// Seed even intentionally inconsistent states, such as a deleted parent.
	for _, record := range records {
		msg, err := decodeArchiveRecord(record)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		kind, err := findArchiveKind(record.Kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(kind.bucket, record.ID, raw); err != nil {
			t.Fatal(err)
		}
	}
}

func TestArchiveReplaceOriginalAcceptsNewSourceGeneration(t *testing.T) {
	existing, incoming, opts := archiveOriginalFixture(t)
	// Source changes, including removal of an unchanged imported message,
	// are the purpose of replacement and must not look like destination edits.
	incoming = slices.DeleteFunc(incoming, func(record archive.Record) bool { return record.ID == sessionMessageKey("chat", 2) })
	incoming[0] = archiveTestRecord(t, "sessions", "chat", &lobslawv1.SessionRecord{Id: "chat", FirstSeq: 1, NextSeq: 2})
	incoming[1] = archiveTestRecord(t, "session-messages", sessionMessageKey("chat", 1), &lobslawv1.SessionMessage{SessionId: "chat", Seq: 1, Content: "new generation"})
	plan, err := PlanArchiveImport(existing, incoming, opts)
	if err != nil || len(plan.Conflicts) != 0 || len(plan.Replaced) != 2 {
		t.Fatalf("new source generation rejected: %+v %v", plan, err)
	}
}
