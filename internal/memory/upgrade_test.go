package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
	"google.golang.org/protobuf/proto"

	"github.com/hashicorp/raft"

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
	if err != nil || state.Prepared != nil || state.Active != uint32(dataformat.StateVersion) {
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

func TestUpgradeConflictDoesNotHaltCommittedFSM(t *testing.T) {
	node, fsm := newTestRaft(t)
	raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_Upgrade{Upgrade: &pb.UpgradeCommand{Action: "abort", TransitionId: "stale", Target: 1, MembershipIndex: 1, MemberIds: []string{"test-node"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := node.applyRaw(raw, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conflict, ok := result.(error)
	if !ok || !errors.Is(conflict, ErrUpgradeConflict) {
		t.Fatal("expected deterministic conflict", result)
	}
	if fsm.Failure() != nil {
		t.Fatal("stale operator command halted the cluster", fsm.Failure())
	}
}

func TestSnapshotCoveredLogGaps(t *testing.T) {
	logs := raft.NewInmemStore()
	for _, index := range []uint64{1, 5} {
		if err := logs.StoreLog(&raft.Log{Index: index, Term: 1, Type: raft.LogNoop}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := preflightLogs(context.Background(), logs, ""); err == nil {
		t.Fatal("accepted unexplained missing logs")
	}
	if _, err := preflightLogs(context.Background(), logs, "", 3); err == nil {
		t.Fatal("accepted a gap beyond the validated snapshot")
	}
	if _, err := preflightLogs(context.Background(), logs, "", 4); err != nil {
		t.Fatal("rejected installed snapshot gap", err)
	}
}

func TestUpgradeConfigurationFingerprintIncludesAddressAndSuffrage(t *testing.T) {
	servers := []raft.Server{{ID: "a", Address: "a:1", Suffrage: raft.Voter}, {ID: "b", Address: "b:1", Suffrage: raft.Voter}}
	digest := configurationFingerprint(servers)
	if digest != configurationFingerprint([]raft.Server{servers[1], servers[0]}) {
		t.Fatal("configuration ordering changed identity")
	}
	changed := slices.Clone(servers)
	changed[0].Address = "other:1"
	if digest == configurationFingerprint(changed) {
		t.Fatal("address change ignored")
	}
	changed = slices.Clone(servers)
	changed[0].Suffrage = raft.Nonvoter
	if digest == configurationFingerprint(changed) {
		t.Fatal("voting change ignored")
	}
}

func TestUpgradeCompletedRetryChecksTarget(t *testing.T) {
	node, fsm := newTestRaft(t)
	completed := dataformat.ContractState{
		Active: 1, Epoch: 1, CompletedID: "aborted", CompletedAction: "abort",
		Completed: &dataformat.Transition{ID: "aborted", Target: 2, MembershipIndex: 1, MembershipFingerprint: configurationFingerprint(node.ConfigurationServers()), Members: []string{"test-node"}, Index: 2},
	}
	raw, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.store.loadDB().Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(formatBucket)).Put([]byte(contractKey), raw) }); err != nil {
		t.Fatal(err)
	}
	if _, err := node.ChangeUpgrade(context.Background(), &pb.ChangeUpgradeRequest{Action: "abort", TransitionId: "aborted", Target: 3}); err == nil {
		t.Fatal("different target accepted as completed retry")
	}
	if _, err := node.ChangeUpgrade(context.Background(), &pb.ChangeUpgradeRequest{Action: "abort", TransitionId: "aborted", Target: 2}); err != nil {
		t.Fatal("exact retry rejected", err)
	}
	completed.Completed.MembershipFingerprint = "prior membership"
	raw, err = json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.store.loadDB().Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte(formatBucket)).Put([]byte(contractKey), raw) }); err != nil {
		t.Fatal(err)
	}
	if _, err := node.ChangeUpgrade(context.Background(), &pb.ChangeUpgradeRequest{Action: "abort", TransitionId: "aborted", Target: 2}); err == nil {
		t.Fatal("retry accepted after membership changed")
	}

}
