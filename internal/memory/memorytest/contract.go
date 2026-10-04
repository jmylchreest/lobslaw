// Package memorytest provides explicit persisted-contract fixtures for service tests.
package memorytest

import (
	"testing"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// ActivateTeams constructs an already-activated fixture before a test attaches
// its synthetic log. Production upgrades must use operator control and Raft.
func ActivateTeams(t testing.TB, store *memory.Store) {
	t.Helper()
	state, err := store.ContractState()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active >= 2 {
		return
	}
	fsm := memory.NewFSM(store)
	for i, action := range []string{"prepare", "finalize"} {
		raw, err := proto.Marshal(&pb.LogEntry{Op: pb.LogOp_LOG_OP_PUT, Payload: &pb.LogEntry_Upgrade{Upgrade: &pb.UpgradeCommand{Action: action, TransitionId: "fixture", Target: 2, MembershipIndex: 1, MemberIds: []string{"fixture"}}}})
		if err != nil {
			t.Fatal(err)
		}
		if result := fsm.Apply(&raft.Log{Index: uint64(i + 1), Data: raw}); result != nil {
			t.Fatal(result)
		}
	}
	if err := store.Delete(memory.BucketRaftMeta, memory.KeyLastAppliedIndex); err != nil {
		t.Fatal(err)
	}
}
