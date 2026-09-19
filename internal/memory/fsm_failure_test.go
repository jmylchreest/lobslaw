package memory

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/encoding/protowire"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestUnsupportedEntryStopsRaft(t *testing.T) {
	node, fsm := newTestRaft(t)
	data := mustMarshalEntry(t, &lobslawv1.LogEntry{Op: lobslawv1.LogOp(999)})
	response, err := node.Apply(data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if failure, ok := response.(error); !ok || !errors.Is(failure, ErrFSMUnsupported) {
		t.Fatalf("response: %v", response)
	}
	select {
	case <-fsm.Failed():
	case <-time.After(5 * time.Second):
		t.Fatal("no failure signal")
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for node.Raft.State() != raft.Shutdown {
		select {
		case <-deadline:
			t.Fatal("Raft did not stop")
		case <-ticker.C:
		}
	}
}

type failureSnapshotSink struct {
	bytes.Buffer
	cancelled bool
	closed    bool
}

func (s *failureSnapshotSink) ID() string    { return "test" }
func (s *failureSnapshotSink) Cancel() error { s.cancelled = true; return nil }
func (s *failureSnapshotSink) Close() error  { s.closed = true; return nil }

func TestPendingSnapshotCannotPublishAfterFSMFailure(t *testing.T) {
	fsm := NewFSM(replayStore(t))
	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()
	fsm.Apply(&raft.Log{Index: 2, Data: []byte{0xff}})
	sink := &failureSnapshotSink{}
	if err := snap.Persist(sink); !errors.Is(err, ErrFSMUnsupported) {
		t.Fatalf("persist: %v", err)
	}
	if !sink.cancelled || sink.closed || sink.Len() != 0 {
		t.Fatal("failed snapshot published")
	}
}

func TestUnsupportedEntryRemainsReplayableAfterRestart(t *testing.T) {
	store := replayStore(t)
	unsupported := &raft.Log{Index: 7, Data: mustMarshalEntry(t, &lobslawv1.LogEntry{Op: lobslawv1.LogOp(999)})}
	NewFSM(store).Apply(unsupported)
	restarted := NewFSM(store)
	if err, ok := restarted.Apply(unsupported).(error); !ok || !errors.Is(err, ErrFSMUnsupported) {
		t.Fatalf("incompatible restart skipped entry: %v", err)
	}
	// Model a supported decoder after upgrade by supplying a understood entry
	// at the same log index. Production never edits the persisted log bytes.
	upgraded := NewFSM(store)
	valid := mustMarshalEntry(t, &lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "recovered", Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{Id: "recovered"}}})
	if err, ok := upgraded.Apply(&raft.Log{Index: 7, Data: valid}).(error); ok && err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(BucketCommitments, "recovered"); err != nil {
		t.Fatal(err)
	}
}

func TestKnownEntryOptionalFieldsAndCASConflictStillAdvance(t *testing.T) {
	fsm := NewFSM(replayStore(t))
	valid := mustMarshalEntry(t, &lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_PUT, Id: "c", Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{Id: "c"}}})
	valid = protowire.AppendTag(valid, 999, protowire.VarintType)
	valid = protowire.AppendVarint(valid, 1)
	if err, ok := fsm.Apply(&raft.Log{Index: 1, Data: valid}).(error); ok && err != nil {
		t.Fatal(err)
	}
	wrongRevision := uint64(999)
	claim := mustMarshalEntry(t, &lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: "c", ExpectedRevision: &wrongRevision, Payload: &lobslawv1.LogEntry_Commitment{Commitment: &lobslawv1.AgentCommitment{Id: "c"}}})
	if err, ok := fsm.Apply(&raft.Log{Index: 2, Data: claim}).(error); !ok || !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("claim: %v", err)
	}
	if fsm.lastApplied() != 2 || fsm.Failure() != nil {
		t.Fatalf("CAS rejection halted progress: index=%d failure=%v", fsm.lastApplied(), fsm.Failure())
	}
	if err, ok := fsm.Apply(&raft.Log{Index: 3, Data: valid}).(error); ok && err != nil {
		t.Fatal(err)
	}
	if fsm.lastApplied() != 3 {
		t.Fatal("valid entry after conflict skipped")
	}
}

// The failure can arrive after Persist's initial check, while bytes are copied.
type failingDuringCopySink struct {
	failureSnapshotSink
	fail func()
}

func (s *failingDuringCopySink) Write(data []byte) (int, error) {
	if s.fail != nil {
		fail := s.fail
		s.fail = nil
		fail()
	}
	return s.Buffer.Write(data)
}
func TestSnapshotCopyCannotPublishAfterFSMFailure(t *testing.T) {
	fsm := NewFSM(replayStore(t))
	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()
	sink := &failingDuringCopySink{fail: func() { fsm.Apply(&raft.Log{Index: 2, Data: []byte{0xff}}) }}
	if err := snap.Persist(sink); !errors.Is(err, ErrFSMUnsupported) {
		t.Fatalf("persist: %v", err)
	}
	if !sink.cancelled || sink.closed {
		t.Fatal("snapshot published after failure during copy")
	}
}
