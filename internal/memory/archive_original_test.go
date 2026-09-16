package memory

import (
	"context"
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
