package memory

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/hashicorp/raft"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func historyTask(id, owner, actor string) *pb.TaskApprovalRecord {
	return &pb.TaskApprovalRecord{Id: id, Owner: owner, Actor: actor, CoordinatorConversation: true, Transcript: []*pb.SessionMessage{
		{Role: "user", Content: "question " + id},
		{Role: "assistant", ToolCalls: []*pb.SessionToolCall{{Id: "call-" + id, Name: "echo", Arguments: `{}`}}},
		{Role: "tool", ToolCallId: "call-" + id, Content: "result " + id},
		{Role: "assistant", Content: "answer " + id},
	}}
}

func putHistoryTask(t *testing.T, s *Store, task *pb.TaskApprovalRecord) {
	t.Helper()
	raw, err := proto.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketTaskApprovals, task.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestTaskHistoryBoundedMixedOwnersAndActors(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	const total = 6000
	// Poison evidence outside the requested window after indexing. Any full
	// scan, cross-owner read or extra decode fails, regardless of machine speed.
	err := s.loadDB().Update(func(tx *bolt.Tx) error {
		for i := range total {
			owner, actor := "user:alice", "bot:chief"
			switch i % 3 {
			case 1:
				owner = "user:bob"
			case 2:
				actor = "bot:worker"
			}
			task := historyTask(fmt.Sprintf("%08d", i), owner, actor)
			raw, err := proto.Marshal(task)
			if err != nil {
				return err
			}
			if err := s.updateTaskHistory(tx, task.Id, raw); err != nil {
				return err
			}
			sealed := []byte("must not decrypt this record")
			if i%3 == 0 && i >= total-75 {
				sealed, err = s.cipher.Seal(raw)
				if err != nil {
					return err
				}
			}
			if err := tx.Bucket([]byte(BucketTaskApprovals)).Put([]byte(task.Id), sealed); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.CoordinatorTaskHistory("user:alice", "bot:chief")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 25 || tasks[0].Id != "00005925" || tasks[24].Id != "00005997" {
		t.Fatalf("wrong bounded chronological window: %v", tasks)
	}
	for _, task := range tasks {
		if !proto.Equal(task, historyTask(task.Id, "user:alice", "bot:chief")) {
			t.Fatalf("changed full transcript: %v", task)
		}
	}
	for _, scope := range [][2]string{{"", "bot:chief"}, {"user:alice", ""}, {"user:ali", "bot:chief"}, {"user:alice", "bot:chie"}, {"user:alice/bot", "chief"}} {
		got, err := s.CoordinatorTaskHistory(scope[0], scope[1])
		if err != nil || len(got) != 0 {
			t.Fatalf("scope %v leaked: %v %v", scope, got, err)
		}
	}
}

func TestTaskHistoryWholeBatchesAndByteBounds(t *testing.T) {
	t.Parallel()
	t.Run("whole batches cross target", func(t *testing.T) {
		s, _ := newTestStore(t)
		for i := range 40 {
			task := historyTask(fmt.Sprintf("%08d", i), "user:alice", "bot:chief")
			task.Transcript = task.Transcript[:3]
			putHistoryTask(t, s, task)
		}
		tasks, err := s.CoordinatorTaskHistory("user:alice", "bot:chief")
		if err != nil || len(tasks) != 34 || len(tasks[0].Transcript) != 3 {
			t.Fatalf("split a batch: %d %v", len(tasks), err)
		}
	})
	t.Run("bytes stop before decrypt", func(t *testing.T) {
		s, _ := newTestStore(t)
		for i := range 6 {
			task := historyTask(fmt.Sprint(i), "user:alice", "bot:chief")
			task.Transcript[0].Content = strings.Repeat("x", maxTaskCheckpoint)
			putHistoryTask(t, s, task)
		}
		tasks, err := s.CoordinatorTaskHistory("user:alice", "bot:chief")
		if err != nil || len(tasks) != 3 || tasks[0].Id != "3" || tasks[2].Id != "5" {
			t.Fatalf("byte bound: %d %v", len(tasks), err)
		}
		// Invalid oversized ciphertext proves the bound is checked before decode.
		err = s.loadDB().Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte(BucketTaskApprovals)).Put([]byte("5"), bytes.Repeat([]byte("x"), TaskHistoryMaxBytes+1))
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CoordinatorTaskHistory("user:alice", "bot:chief"); err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("oversized latest evidence: %v", err)
		}
	})
}

func TestTaskHistoryFSMUpdatesReplayAndDelete(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	fsm := NewFSM(s)
	task := historyTask("task", "user:alice", "bot:chief")
	apply := func(index uint64, op pb.LogOp, expected *uint64) *raft.Log {
		t.Helper()
		raw, err := proto.Marshal(&pb.LogEntry{Id: task.Id, Op: op, ExpectedRevision: expected, Payload: &pb.LogEntry_TaskApproval{TaskApproval: task}})
		if err != nil {
			t.Fatal(err)
		}
		log := &raft.Log{Index: index, Data: raw}
		if err, ok := fsm.Apply(log).(error); ok {
			t.Fatal(err)
		}
		return log
	}
	first := apply(1, pb.LogOp_LOG_OP_PUT, nil)
	task.Transcript[3].Content = "resumed answer"
	task.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_COMPLETED
	apply(2, pb.LogOp_LOG_OP_CLAIM, proto.Uint64(1))
	if err, ok := NewFSM(s).Apply(first).(error); ok {
		t.Fatal(err)
	}
	got, err := s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 1 || got[0].Transcript[3].Content != "resumed answer" {
		t.Fatalf("lost completion after replay: %v %v", got, err)
	}
	// Eligibility is admission-time classification, not current profile/state.
	task.State = pb.TaskApprovalState_TASK_APPROVAL_STATE_OUTCOME_UNKNOWN
	apply(3, pb.LogOp_LOG_OP_CLAIM, proto.Uint64(2))
	got, err = s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 1 {
		t.Fatalf("uncertain evidence lost: %v %v", got, err)
	}
	task.Owner, task.Actor = "user:bob", "bot:other"
	apply(4, pb.LogOp_LOG_OP_PUT, nil)
	got, err = s.CoordinatorTaskHistory("user:alice", "bot:chief")
	if err != nil || len(got) != 0 {
		t.Fatalf("stale owner index: %v %v", got, err)
	}
	got, err = s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 1 {
		t.Fatalf("missing replacement index: %v %v", got, err)
	}
	apply(5, pb.LogOp_LOG_OP_DELETE, nil)
	got, err = s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 0 {
		t.Fatalf("stale deleted index: %v %v", got, err)
	}
}

func TestTaskHistoryLegacyReopenAndSnapshot(t *testing.T) {
	t.Parallel()
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale-index-%t", stale), func(t *testing.T) {
			s, path := newTestStore(t)
			task := historyTask("legacy", "user:alice", "bot:chief")
			putHistoryTask(t, s, task)
			// Old snapshots have either no index or a stale one after downgrade.
			err := s.loadDB().Update(func(tx *bolt.Tx) error {
				if err := tx.DeleteBucket([]byte(bucketTaskHistory)); err != nil {
					return err
				}
				if stale {
					_, err := tx.CreateBucket([]byte(bucketTaskHistory))
					return err
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var snapshot bytes.Buffer
			if err := s.WriteSnapshot(&snapshot); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(path, s.key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			assertHistory := func() {
				t.Helper()
				got, err := reopened.CoordinatorTaskHistory(task.Owner, task.Actor)
				if err != nil || len(got) != 1 || !proto.Equal(got[0], task) {
					t.Fatalf("legacy evidence not rebuilt: %v %v", got, err)
				}
			}
			assertHistory()
			putHistoryTask(t, reopened, historyTask("post-snapshot", task.Owner, task.Actor))
			if err := NewFSM(reopened).Restore(io.NopCloser(bytes.NewReader(snapshot.Bytes()))); err != nil {
				t.Fatal(err)
			}
			assertHistory()
		})
	}
}

func TestTaskHistoryAdmissionIsAtomic(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	fsm := NewFSM(s)
	task := historyTask("task", "user:alice", "bot:chief")
	item := &pb.BotInboxItem{Id: "inbox", Recipient: "chief"}
	if err := fsm.putTaskAdmission(task, item, inboxKey(item.Recipient, item.Id)); err != nil {
		t.Fatal(err)
	}
	got, err := s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 1 {
		t.Fatalf("admission omitted index: %v %v", got, err)
	}
	if err := s.loadDB().Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte(BucketBotInbox)) }); err != nil {
		t.Fatal(err)
	}
	task.Id = "rejected"
	if err := fsm.putTaskAdmission(task, item, inboxKey(item.Recipient, item.Id)); err == nil {
		t.Fatal("expected failed admission")
	}
	got, err = s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 1 || got[0].Id != "task" {
		t.Fatalf("failed admission left index or evidence: %v %v", got, err)
	}
}

func TestTaskHistoryEligibilityAndHardMessageLimit(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	task := historyTask("task", "user:alice", "bot:chief")
	putHistoryTask(t, s, task)
	task.CoordinatorConversation = false
	putHistoryTask(t, s, task)
	got, err := s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 0 {
		t.Fatalf("specialist evidence leaked: %v %v", got, err)
	}
	// Removing a derived marker must not remove the durable evidence.
	if _, err := s.Get(BucketTaskApprovals, task.Id); err != nil {
		t.Fatal(err)
	}
	task.CoordinatorConversation = true
	task.Transcript = nil
	putHistoryTask(t, s, task)
	got, err = s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty evidence indexed: %v %v", got, err)
	}
	for range TaskHistoryMaxMessages + 1 {
		task.Transcript = append(task.Transcript, &pb.SessionMessage{Role: "user"})
	}
	putHistoryTask(t, s, task)
	if _, err := s.CoordinatorTaskHistory(task.Owner, task.Actor); err == nil || !strings.Contains(err.Error(), "message limit") {
		t.Fatalf("oversized latest transcript silently omitted: %v", err)
	}
}

func TestTaskHistoryFailedSnapshotRebuildPreservesLiveStore(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	task := historyTask("task", "user:alice", "bot:chief")
	putHistoryTask(t, s, task)
	bad, _ := newTestStore(t)
	if err := bad.loadDB().Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(BucketTaskApprovals)).Put([]byte("corrupt"), []byte("not ciphertext"))
	}); err != nil {
		t.Fatal(err)
	}
	var snapshot bytes.Buffer
	if err := bad.WriteSnapshot(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFromSnapshot(&snapshot); err == nil {
		t.Fatal("published corrupt task evidence")
	}
	got, err := s.CoordinatorTaskHistory(task.Owner, task.Actor)
	if err != nil || len(got) != 1 || !proto.Equal(got[0], task) {
		t.Fatalf("failed rebuild displaced live history: %v %v", got, err)
	}
}
