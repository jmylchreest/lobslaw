package memory

import (
	"path/filepath"
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// replayStore is a bbolt store on disk, which is the whole point: the
// bug only exists because this state OUTLIVES the process.
func replayStore(t *testing.T) *Store {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// This FSM's state is DURABLE, and hashicorp/raft does not assume that.
//
// raft replays the log from the last snapshot — index 1 when there is
// no snapshot — on the premise that the FSM is either in memory or has
// just been rebuilt by Restore. A bbolt file that survived the restart
// already holding the final state gets the whole of history re-applied
// on top of it.
//
// The user-visible symptom was a one-shot "remind me in 30 seconds"
// commitment firing again on every restart, for hours: it had been
// marked done correctly, in raft, and the mark was undone by the next
// boot replaying the PUT that created it. Diagnosed by tracing a live
// node, where every replayed CLAIM arrived with expectedRev 0,1,2,3
// against a store already at revision 4.
func TestReplayDoesNotResurrectSupersededState(t *testing.T) {
	t.Parallel()

	store := replayStore(t)
	fsm := NewFSM(store)

	pending := &lobslawv1.AgentCommitment{
		Id:         "c1",
		Status:     "pending",
		HandlerRef: "agent:turn",
		DueAt:      timestamppb.Now(),
	}
	putEntry := mustMarshalEntry(t, &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      "c1",
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: pending},
	})

	// Index 1: the commitment is created.
	if err, ok := fsm.Apply(&raft.Log{Index: 1, Data: putEntry}).(error); ok && err != nil {
		t.Fatalf("apply put: %v", err)
	}
	rev := currentRevision(t, store, "c1")

	// Index 2: it fires and is marked done, the CAS naming the
	// revision it read.
	done := proto.Clone(pending).(*lobslawv1.AgentCommitment)
	done.Status = "done"
	doneEntry := mustMarshalEntry(t, &lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               "c1",
		Payload:          &lobslawv1.LogEntry_Commitment{Commitment: done},
		ExpectedClaimer:  "",
		ExpectedRevision: &rev,
	})
	if err, ok := fsm.Apply(&raft.Log{Index: 2, Data: doneEntry}).(error); ok && err != nil {
		t.Fatalf("apply done: %v", err)
	}
	if got := statusOf(t, store, "c1"); got != "done" {
		t.Fatalf("status before restart = %q; want done", got)
	}

	// The restart. The bbolt file persists, so a fresh FSM opens the
	// SAME store — and raft, having no snapshot, replays from index 1.
	replayed := NewFSM(store)
	for i, data := range [][]byte{putEntry, doneEntry} {
		replayed.Apply(&raft.Log{Index: uint64(i + 1), Data: data})
	}

	// Without the last-applied guard the PUT at index 1 restores
	// "pending", and the CLAIM at index 2 then fails its CAS because
	// the revision has moved on — so the record is left pending and the
	// commitment fires again.
	if got := statusOf(t, store, "c1"); got != "done" {
		t.Errorf("status after replay = %q; want done — replay resurrected superseded state", got)
	}
}

// The guard must not stop a genuinely new entry from applying, which is
// the way a fix like this breaks everything quietly.
func TestApplyStillAcceptsEntriesBeyondLastApplied(t *testing.T) {
	t.Parallel()

	store := replayStore(t)
	fsm := NewFSM(store)

	first := mustMarshalEntry(t, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "c1",
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{
			Id: "c1", Status: "pending", HandlerRef: "agent:turn"}},
	})
	fsm.Apply(&raft.Log{Index: 7, Data: first})

	second := mustMarshalEntry(t, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "c2",
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{
			Id: "c2", Status: "pending", HandlerRef: "agent:turn"}},
	})
	if err, ok := fsm.Apply(&raft.Log{Index: 8, Data: second}).(error); ok && err != nil {
		t.Fatalf("apply index 8: %v", err)
	}
	if _, err := store.Get(BucketCommitments, "c2"); err != nil {
		t.Error("an entry past last-applied was skipped; the guard is too broad")
	}

	// And an index at or below it is skipped rather than re-applied.
	if _, err := store.Get(BucketCommitments, "c1"); err != nil {
		t.Fatal("setup: c1 missing")
	}
	if err := store.Delete(BucketCommitments, "c1"); err != nil {
		t.Fatal(err)
	}
	fsm.Apply(&raft.Log{Index: 7, Data: first})
	if _, err := store.Get(BucketCommitments, "c1"); err == nil {
		t.Error("index 7 was re-applied after last-applied reached 8")
	}
}

// Index 0 never comes from raft — its first index is 1 — so it means a
// direct caller, and those must always apply. Tests across this package
// build a raft.Log by hand; a guard that swallowed them would turn the
// FSM into a silent no-op for every caller that is not raft itself.
func TestApplyAlwaysRunsForSyntheticIndexZero(t *testing.T) {
	t.Parallel()

	store := replayStore(t)
	fsm := NewFSM(store)

	entry := mustMarshalEntry(t, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "c9",
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{
			Id: "c9", Status: "pending", HandlerRef: "agent:turn"}},
	})
	fsm.Apply(&raft.Log{Index: 50, Data: entry})
	if err := store.Delete(BucketCommitments, "c9"); err != nil {
		t.Fatal(err)
	}
	fsm.Apply(&raft.Log{Data: entry}) // Index 0
	if _, err := store.Get(BucketCommitments, "c9"); err != nil {
		t.Error("a synthetic index-0 entry was skipped")
	}
}

func mustMarshalEntry(t *testing.T, e *lobslawv1.LogEntry) []byte {
	t.Helper()
	b, err := proto.Marshal(e)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	return b
}

func statusOf(t *testing.T, s *Store, id string) string {
	t.Helper()
	raw, err := s.Get(BucketCommitments, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	var c lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal %s: %v", id, err)
	}
	return c.Status
}

func currentRevision(t *testing.T, s *Store, id string) uint64 {
	t.Helper()
	raw, err := s.Get(BucketCommitments, id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	var c lobslawv1.AgentCommitment
	if err := proto.Unmarshal(raw, &c); err != nil {
		t.Fatalf("unmarshal %s: %v", id, err)
	}
	return c.Revision
}

func TestUnsupportedEntryMustNotAdvanceOrAllowLaterWrites(t *testing.T) {
	unknownPayload := mustMarshalEntry(t, &lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "future"})
	unknownPayload = protowire.AppendTag(unknownPayload, 999, protowire.BytesType)
	unknownPayload = protowire.AppendBytes(unknownPayload, []byte{})
	for name, data := range map[string][]byte{
		"unknown operation":  mustMarshalEntry(t, &lobslawv1.LogEntry{Op: lobslawv1.LogOp(999)}),
		"unknown payload":    unknownPayload,
		"malformed protobuf": {0xff},
	} {
		t.Run(name, func(t *testing.T) {
			store := replayStore(t)
			fsm := NewFSM(store)
			valid := mustMarshalEntry(t, &lobslawv1.LogEntry{
				Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "later",
				Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{Id: "later"}},
			})
			if err, ok := fsm.Apply(&raft.Log{Index: 2, Data: data}).(error); !ok || err == nil {
				t.Fatal("unsupported entry accepted")
			}
			if got := fsm.lastApplied(); got != 0 {
				t.Errorf("advanced past unsupported entry: %d", got)
			}
			fsm.Apply(&raft.Log{Index: 3, Data: valid})
			if _, err := store.Get(BucketCommitments, "later"); err == nil {
				t.Error("later entry applied after unsupported entry")
			}
			if got := NewFSM(store).lastApplied(); got != 0 {
				t.Errorf("restart skips unsupported entry: durable index %d", got)
			}
			snap, err := fsm.Snapshot()
			if err == nil {
				snap.Release()
				t.Error("snapshot accepted after unsupported entry")
			}
		})
	}
}
