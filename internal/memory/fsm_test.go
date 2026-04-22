package memory

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func newTestRaft(t *testing.T) (*RaftNode, *FSM) {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	fsm := NewFSM(store)
	node, err := NewRaft(RaftConfig{
		NodeID:            "test-node",
		DataDir:           dir,
		Bootstrap:         true,
		InMemoryTransport: true,
	}, fsm)
	if err != nil {
		t.Fatalf("NewRaft: %v", err)
	}
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatalf("WaitForLeader: %v", err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	return node, fsm
}

func TestFSMApplyPutPolicyRule(t *testing.T) {
	t.Parallel()
	node, fsm := newTestRaft(t)

	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: "rule-1",
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{
				Id:       "rule-1",
				Subject:  "user:alice",
				Action:   "memory:read",
				Resource: "*",
				Effect:   "allow",
				Priority: 10,
			},
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	res, err := node.Apply(data, 2*time.Second)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res != nil {
		t.Errorf("Apply returned error: %v", res)
	}

	// Read back from the store.
	raw, err := fsm.Store().Get(BucketPolicyRules, "rule-1")
	if err != nil {
		t.Fatalf("Store.Get: %v", err)
	}
	var got lobslawv1.PolicyRule
	if err := proto.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Subject != "user:alice" {
		t.Errorf("subject = %q, want user:alice", got.Subject)
	}
}

func TestFSMApplyDelete(t *testing.T) {
	t.Parallel()
	node, fsm := newTestRaft(t)

	// Seed.
	put := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: "rule-2",
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{Id: "rule-2", Effect: "deny"},
		},
	}
	data, _ := proto.Marshal(put)
	if _, err := node.Apply(data, time.Second); err != nil {
		t.Fatal(err)
	}

	// Delete.
	del := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_DELETE,
		Id: "rule-2",
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{Id: "rule-2"},
		},
	}
	data, _ = proto.Marshal(del)
	if _, err := node.Apply(data, time.Second); err != nil {
		t.Fatal(err)
	}

	if _, err := fsm.Store().Get(BucketPolicyRules, "rule-2"); err == nil {
		t.Error("rule-2 should have been deleted")
	}
}

func TestFSMSnapshotRestore(t *testing.T) {
	t.Parallel()
	node, fsm := newTestRaft(t)

	// Apply a few entries so the FSM has state.
	for _, id := range []string{"a", "b", "c"} {
		entry := &lobslawv1.LogEntry{
			Op: lobslawv1.LogOp_LOG_OP_PUT,
			Id: id,
			Payload: &lobslawv1.LogEntry_PolicyRule{
				PolicyRule: &lobslawv1.PolicyRule{Id: id, Subject: "s:" + id},
			},
		}
		data, _ := proto.Marshal(entry)
		if _, err := node.Apply(data, time.Second); err != nil {
			t.Fatal(err)
		}
	}

	// Snapshot via raft.
	future := node.Raft.Snapshot()
	if err := future.Error(); err != nil {
		t.Fatalf("Raft.Snapshot: %v", err)
	}

	// Verify all three records are retrievable after the snapshot.
	for _, id := range []string{"a", "b", "c"} {
		if _, err := fsm.Store().Get(BucketPolicyRules, id); err != nil {
			t.Errorf("after snapshot %q missing: %v", id, err)
		}
	}
	_ = raft.ErrCantBootstrap // keep raft import honest
}

func TestFSMRejectsUnknownOp(t *testing.T) {
	t.Parallel()
	node, _ := newTestRaft(t)
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_UNSPECIFIED,
		Id: "bad",
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{Id: "bad"},
		},
	}
	data, _ := proto.Marshal(entry)
	res, err := node.Apply(data, time.Second)
	if err != nil {
		t.Fatalf("Apply returned unexpected error: %v", err)
	}
	if res == nil {
		t.Error("FSM should have returned an error in Apply response for unknown op")
	}
}
