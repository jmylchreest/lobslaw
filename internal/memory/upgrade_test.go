package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestUpgradeControlCannotUseOrdinaryProposal(t *testing.T) {
	node, fsm := newTestRaft(t)
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_Upgrade{Upgrade: &pb.UpgradeCommand{Action: "prepare", Target: 2, TransitionId: "bypass"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := node.Apply(raw, time.Second); err == nil {
		t.Fatal("accepted upgrade through ordinary/forwarded proposal")
	}
	if fsm.Failure() != nil {
		t.Fatal("invalid proposal reached committed FSM", fsm.Failure())
	}
	state, err := fsm.store.ContractState()
	if err != nil || state.Prepared != nil || state.Active != 1 {
		t.Fatal(state, err)
	}
	if _, err := node.ChangeUpgrade(context.Background(), &pb.ChangeUpgradeRequest{Action: "prepare", TransitionId: "unsupported", Target: 99}); err == nil {
		t.Fatal("accepted unknown target")
	}
}

func TestUpgradeFenceRefusesOlderStartupWithoutMutation(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenStore(path, key)
	if err != nil {
		t.Fatal(err)
	}
	future := dataformat.ContractState{Active: 1, Prepared: &dataformat.Transition{ID: "pending", Target: 99, MembershipIndex: 1, Members: []string{"a"}, Index: 2}}
	raw, err := json.Marshal(future)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.loadDB().Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(formatBucket)).Put([]byte(contractKey), raw) }); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenStore(path, key); err == nil {
		_ = opened.Close()
		t.Fatal("ignored durable restart fence")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected startup modified source")
	}
}

func TestSnapshotFreezesStateAtCapture(t *testing.T) {
	store := replayStore(t)
	fsm := NewFSM(store)
	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()
	if err := store.Put(BucketPolicyRules, "after-capture", []byte("later")); err != nil {
		t.Fatal(err)
	}
	sink := &failureSnapshotSink{}
	if err := snap.Persist(sink); err != nil {
		t.Fatal(err)
	}
	destination, err := OpenStore(filepath.Join(t.TempDir(), "state.db"), store.key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = destination.Close() }()
	if err := destination.RestoreFromSnapshot(bytes.NewReader(sink.Bytes())); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.Get(BucketPolicyRules, "after-capture"); err == nil {
		t.Fatal("snapshot contains writes after its Raft capture index")
	}
}
