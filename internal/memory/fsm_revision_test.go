package memory

import (
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The claim CAS used to compare claimed_by and nothing else, and every
// write replaced the whole record from the writer's own read. That
// combination produced three distinct bugs, all of which reduce to the
// same thing: a writer working from a read that had since gone stale
// could not be told apart from one working from current state.
//
// claimed_by cannot make that distinction on its own, because
// "nobody holds this" and "somebody held it, did the work, and
// released it" are the same value.

func loadTaskRec(t *testing.T, f *FSM, id string) *lobslawv1.ScheduledTaskRecord {
	t.Helper()
	raw, err := f.Store().Get(BucketScheduledTasks, id)
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	var rec lobslawv1.ScheduledTaskRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	return &rec
}

func claimTask(t *testing.T, f *FSM, rec *lobslawv1.ScheduledTaskRecord, expectClaimer string, expectRev uint64) error {
	t.Helper()
	res := applyEntry(t, f, &lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               rec.Id,
		Payload:          &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: rec},
		ExpectedClaimer:  expectClaimer,
		ExpectedRevision: new(expectRev),
	})
	if err, ok := res.(error); ok {
		return err
	}
	return nil
}

// TestClaimRejectsWriterWorkingFromAStaleRead is the double-fire. Two
// schedulers scan concurrently and both see an unclaimed, due task. A
// claims, runs it, and completes — which clears the claim. B then
// claims from its original read, and under the old CAS succeeded,
// because claimed_by was "" again. The task fired twice.
func TestClaimRejectsWriterWorkingFromAStaleRead(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	seed := &lobslawv1.ScheduledTaskRecord{
		Id: "task", Schedule: "* * * * *", Enabled: true,
		NextRun: timestamppb.New(time.Now().Add(-time.Minute)),
	}
	applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "task",
		Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: seed},
	})

	// Both schedulers read the same record.
	readByA := loadTaskRec(t, fsm, "task")
	readByB := loadTaskRec(t, fsm, "task")

	// A claims, then completes: clears the claim and advances NextRun.
	claimed := proto.Clone(readByA).(*lobslawv1.ScheduledTaskRecord)
	claimed.ClaimedBy = "node-a"
	claimed.ClaimExpiresAt = timestamppb.New(time.Now().Add(time.Minute))
	if err := claimTask(t, fsm, claimed, "", readByA.Revision); err != nil {
		t.Fatalf("A's claim failed: %v", err)
	}

	done := proto.Clone(claimed).(*lobslawv1.ScheduledTaskRecord)
	done.ClaimedBy = ""
	done.ClaimExpiresAt = nil
	done.LastRun = timestamppb.Now()
	done.NextRun = timestamppb.New(time.Now().Add(time.Minute))
	if err := claimTask(t, fsm, done, "node-a", readByA.Revision+1); err != nil {
		t.Fatalf("A's completion failed: %v", err)
	}

	// B now claims from the read it took before any of that. The
	// record is unclaimed again, so claimed_by cannot catch this.
	stale := proto.Clone(readByB).(*lobslawv1.ScheduledTaskRecord)
	stale.ClaimedBy = "node-b"
	stale.ClaimExpiresAt = timestamppb.New(time.Now().Add(time.Minute))
	err := claimTask(t, fsm, stale, "", readByB.Revision)
	if err == nil {
		t.Fatal("a claim built from a pre-completion read was accepted; the task would fire twice")
	}
	if !errors.Is(err, ErrClaimConflict) {
		t.Errorf("want ErrClaimConflict, got %v", err)
	}

	// And the completion must still be intact — under the old code B's
	// write also rolled NextRun back to the past, so the task would
	// have fired again on the very next scan.
	after := loadTaskRec(t, fsm, "task")
	if after.LastRun == nil {
		t.Error("A's completion was overwritten by the stale claimer")
	}
	if after.ClaimedBy != "" {
		t.Errorf("claimed_by = %q, want empty", after.ClaimedBy)
	}
}

// TestCompletionCannotClobberAConcurrentEdit is the third bug: the
// completion path writes a record cloned at *claim* time, so anything
// an operator changed while the handler ran was silently reverted.
func TestCompletionCannotClobberAConcurrentEdit(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "task",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: "task", Enabled: true, Name: "nightly"},
		},
	})
	read := loadTaskRec(t, fsm, "task")

	claimed := proto.Clone(read).(*lobslawv1.ScheduledTaskRecord)
	claimed.ClaimedBy = "node-a"
	if err := claimTask(t, fsm, claimed, "", read.Revision); err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Operator disables the task while the handler is running.
	current := loadTaskRec(t, fsm, "task")
	edit := proto.Clone(current).(*lobslawv1.ScheduledTaskRecord)
	edit.Enabled = false
	applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "task",
		Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: edit},
	})

	// The completion, built at claim time, still thinks Enabled=true.
	done := proto.Clone(claimed).(*lobslawv1.ScheduledTaskRecord)
	done.ClaimedBy = ""
	done.LastRun = timestamppb.Now()
	err := claimTask(t, fsm, done, "node-a", read.Revision+1)
	if err == nil {
		t.Fatal("a completion built before a concurrent edit was accepted; the edit would be reverted")
	}
	if !errors.Is(err, ErrClaimConflict) {
		t.Errorf("want ErrClaimConflict, got %v", err)
	}
	if loadTaskRec(t, fsm, "task").Enabled {
		t.Error("the operator's disable was reverted by the stale completion")
	}
}

// A CLAIM with no expected_revision is refused rather than treated as
// unconditional. The uint64 zero value would otherwise hand the unsafe
// behaviour to anyone who forgot the field — the same shape as the
// scopeFilter="" bug memory.Audience exists to prevent.
func TestClaimRequiresAnExpectedRevision(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	res := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "task",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: "task", ClaimedBy: "node-a"},
		},
		ExpectedClaimer: "",
		// ExpectedRevision deliberately unset.
	})
	err, ok := res.(error)
	if !ok || err == nil {
		t.Fatal("a CLAIM with no expected_revision was accepted")
	}
	if _, getErr := fsm.Store().Get(BucketScheduledTasks, "task"); getErr == nil {
		t.Error("the refused claim still wrote a record")
	}
}

// Revisions are FSM-assigned and monotonic per record, so a client
// can always compute the value its successful write produced without
// reading it back — which matters because a forwarded write returns
// no FSM response.
func TestRevisionIsMonotonicPerRecord(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	for want := uint64(1); want <= 3; want++ {
		applyEntry(t, fsm, &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "task",
			Payload: &lobslawv1.LogEntry_ScheduledTask{
				ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: "task"},
			},
		})
		if got := loadTaskRec(t, fsm, "task").Revision; got != want {
			t.Fatalf("after %d writes revision = %d, want %d", want, got, want)
		}
	}

	// A second record keeps its own counter.
	applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "other",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: "other"},
		},
	})
	if got := loadTaskRec(t, fsm, "other").Revision; got != 1 {
		t.Errorf("second record revision = %d, want 1 — revisions are per-record", got)
	}
}
