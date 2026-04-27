package memory

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// newClaimTestStore spins up a Store + FSM in a temp dir. Shared helper
// for the CAS + scheduler-change-callback tests.
func newClaimTestStore(t *testing.T) (*Store, *FSM) {
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
	return store, NewFSM(store)
}

// applyEntry is a test helper that serialises + dispatches through
// FSM.Apply without going through a full Raft node. FSM.Apply
// discards the raft.Log's transport metadata; only the payload
// matters for state-transition testing.
func applyEntry(t *testing.T, f *FSM, entry *lobslawv1.LogEntry) any {
	t.Helper()
	data, err := proto.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return f.Apply(&raft.Log{Data: data})
}

func TestFSMApplyClaimFreshInsertWithEmptyExpected(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	res := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "task-1",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id:             "task-1",
				Name:           "nightly",
				ClaimedBy:      "node-a",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "",
	})
	if err, ok := res.(error); ok && err != nil {
		t.Fatalf("fresh claim failed: %v", err)
	}
}

func TestFSMApplyClaimRejectsMismatch(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	// Initial claim by node-a.
	must := func(res any) {
		t.Helper()
		if err, ok := res.(error); ok && err != nil {
			t.Fatal(err)
		}
	}
	must(applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "task-1",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "task-1", ClaimedBy: "node-a",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "",
	}))

	// node-b tries to claim expecting unclaimed — must be rejected.
	res := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "task-1",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "task-1", ClaimedBy: "node-b",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "",
	})
	err, ok := res.(error)
	if !ok || err == nil {
		t.Fatalf("mismatched expected claimer should fail; got %v", res)
	}
	if !errors.Is(err, ErrClaimConflict) {
		t.Errorf("want ErrClaimConflict; got %v", err)
	}
}

// TestFSMApplyClaimExactCASIgnoresExpiry — the FSM does
// deterministic CAS using the exact stored ClaimedBy. Expiry-based
// "stolen claim" semantics live at the SCHEDULER scan layer, not
// in the FSM apply path (otherwise log replay non-determinism
// silently drops writes — see internal/memory/fsm.go applyClaim
// doc for the full story). A fresh node that wants to take over
// an expired claim MUST pass ExpectedClaimer = the actual stored
// ClaimedBy of the prior holder.
func TestFSMApplyClaimExactCASIgnoresExpiry(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	// Put an expired claim in place.
	expired := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "task-1",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "task-1", ClaimedBy: "crashed-node",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(-time.Hour)),
			},
		},
		ExpectedClaimer: "",
	})
	if err, ok := expired.(error); ok && err != nil {
		t.Fatal(err)
	}

	// Fresh node claims with ExpectedClaimer="" — must FAIL because
	// FSM CAS is exact, not expiry-aware. A real scheduler would
	// pass ExpectedClaimer="crashed-node" instead.
	res := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "task-1",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "task-1", ClaimedBy: "fresh-node",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "",
	})
	if err, ok := res.(error); !ok || err == nil {
		t.Errorf("expected ErrClaimConflict (FSM CAS is exact, no expiry magic); got %v", res)
	} else if !errors.Is(err, ErrClaimConflict) {
		t.Errorf("want ErrClaimConflict, got %v", err)
	}

	// Same fresh node passing the actual stored claimer SUCCEEDS.
	res = applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "task-1",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "task-1", ClaimedBy: "fresh-node",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "crashed-node",
	})
	if err, ok := res.(error); ok && err != nil {
		t.Errorf("exact-CAS take-over with ExpectedClaimer=crashed-node should succeed; got %v", err)
	}
}

func TestFSMApplyClaimReleaseByCurrentOwner(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	must := func(res any) {
		t.Helper()
		if err, ok := res.(error); ok && err != nil {
			t.Fatal(err)
		}
	}
	// node-a claims.
	must(applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "t",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "t", ClaimedBy: "node-a",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "",
	}))

	// node-a releases (expected=node-a, new claim empty).
	must(applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "t",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: "t"},
		},
		ExpectedClaimer: "node-a",
	}))

	// node-b can now claim from empty.
	must(applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "t",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "t", ClaimedBy: "node-b",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "",
	}))
}

func TestFSMApplyClaimCommitmentBucket(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)
	res := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "c-1",
		Payload: &lobslawv1.LogEntry_Commitment{
			Commitment: &lobslawv1.AgentCommitment{
				Id: "c-1", ClaimedBy: "node-a", Status: "pending",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
		ExpectedClaimer: "",
	})
	if err, ok := res.(error); ok && err != nil {
		t.Errorf("commitment CAS should work: %v", err)
	}
}

// TestFSMApplyClaimRejectsNonClaimBucket — CLAIM semantics only
// apply to scheduled_tasks + commitments. A CLAIM op targeting
// policy_rules is a programming error.
func TestFSMApplyClaimRejectsNonClaimBucket(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)
	res := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "r-1",
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{Id: "r-1"},
		},
	})
	if _, ok := res.(error); !ok {
		t.Error("CLAIM on policy_rules should error")
	}
}

func TestFSMSchedulerChangeCallbackFires(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	var fired int
	fsm.SetSchedulerChangeCallback(func() { fired++ })

	// PUT on scheduled_tasks → fire.
	applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: "t-1",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: "t-1", Schedule: "* * * * *"},
		},
	})
	if fired != 1 {
		t.Errorf("fired=%d after scheduled_tasks PUT; want 1", fired)
	}

	// CLAIM on commitments → also fire.
	applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "c-1",
		Payload: &lobslawv1.LogEntry_Commitment{
			Commitment: &lobslawv1.AgentCommitment{
				Id: "c-1", Status: "pending", ClaimedBy: "n",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
	})
	if fired != 2 {
		t.Errorf("fired=%d after commitments CLAIM; want 2", fired)
	}

	// PUT on policy_rules → must NOT fire (scheduler doesn't watch it).
	applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: "r-1",
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: &lobslawv1.PolicyRule{Id: "r-1"},
		},
	})
	if fired != 2 {
		t.Errorf("fired=%d after unrelated PUT; want still 2", fired)
	}
}

// TestFSMSchedulerChangeCallbackSkipsOnFailedApply — the callback
// must only fire when the write actually lands. A rejected CAS
// leaves the store unchanged; the scheduler has nothing new to see.
func TestFSMSchedulerChangeCallbackSkipsOnFailedApply(t *testing.T) {
	t.Parallel()
	_, fsm := newClaimTestStore(t)

	// Seed an existing claim so the next CAS will conflict.
	must := func(res any) {
		if err, ok := res.(error); ok && err != nil {
			t.Fatal(err)
		}
	}
	must(applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "t",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{
				Id: "t", ClaimedBy: "a",
				ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
			},
		},
	}))

	var fired int
	fsm.SetSchedulerChangeCallback(func() { fired++ })

	// Conflicting claim — must error + NOT fire.
	res := applyEntry(t, fsm, &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM,
		Id: "t",
		Payload: &lobslawv1.LogEntry_ScheduledTask{
			ScheduledTask: &lobslawv1.ScheduledTaskRecord{Id: "t", ClaimedBy: "b"},
		},
		ExpectedClaimer: "",
	})
	if _, ok := res.(error); !ok {
		t.Fatal("expected CAS conflict")
	}
	if fired != 0 {
		t.Errorf("failed apply must not fire change callback; fired=%d", fired)
	}
}
