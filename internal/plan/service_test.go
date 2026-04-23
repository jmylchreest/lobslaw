package plan

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// newHarness brings up a single-voter in-proc Raft + a wired Service.
func newHarness(t *testing.T) (*Service, *memory.RaftNode, *memory.Store) {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	fsm := memory.NewFSM(store)
	local := raft.ServerAddress("n1")
	_, inmem := raft.NewInmemTransport(local)
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "n1", LocalAddr: local, DataDir: dir, Bootstrap: true, Transport: inmem,
	}, fsm)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		_ = node.Shutdown()
		_ = store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	return NewService(node, 0), node, store
}

// putCommitment writes a commitment through Raft so the store state
// matches what the real scheduler would see.
func putCommitment(t *testing.T, node *memory.RaftNode, c *lobslawv1.AgentCommitment) {
	t.Helper()
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      c.Id,
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: c},
	}
	data, _ := proto.Marshal(entry)
	res, err := node.Apply(data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		t.Fatal(ferr)
	}
}

func putTask(t *testing.T, node *memory.RaftNode, task *lobslawv1.ScheduledTaskRecord) {
	t.Helper()
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      task.Id,
		Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: task},
	}
	data, _ := proto.Marshal(entry)
	res, err := node.Apply(data, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		t.Fatal(ferr)
	}
}

// --- AddCommitment ---------------------------------------------------------

func TestAddCommitmentHappyPath(t *testing.T) {
	t.Parallel()
	svc, _, _ := newHarness(t)

	resp, err := svc.AddCommitment(context.Background(), &lobslawv1.AddCommitmentRequest{
		Commitment: &lobslawv1.AgentCommitment{
			DueAt:  timestamppb.New(time.Now().Add(time.Hour)),
			Reason: "check on the oven",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Id == "" || len(resp.Id) != 32 {
		t.Errorf("id should be 32 hex chars; got %q", resp.Id)
	}
}

func TestAddCommitmentPreservesCallerID(t *testing.T) {
	t.Parallel()
	svc, _, store := newHarness(t)

	resp, err := svc.AddCommitment(context.Background(), &lobslawv1.AddCommitmentRequest{
		Commitment: &lobslawv1.AgentCommitment{
			Id:     "caller-chosen-id",
			DueAt:  timestamppb.New(time.Now().Add(time.Minute)),
			Reason: "x",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Id != "caller-chosen-id" {
		t.Errorf("caller id should pass through; got %q", resp.Id)
	}

	raw, err := store.Get(memory.BucketCommitments, "caller-chosen-id")
	if err != nil {
		t.Fatal(err)
	}
	var stored lobslawv1.AgentCommitment
	_ = proto.Unmarshal(raw, &stored)
	if stored.Status != "pending" {
		t.Errorf("default status should be pending; got %q", stored.Status)
	}
}

func TestAddCommitmentSanitisesClaimFields(t *testing.T) {
	t.Parallel()
	svc, _, store := newHarness(t)

	// Caller tries to sneak a claim in via the API. Service must
	// strip it — claim state is scheduler-internal.
	resp, err := svc.AddCommitment(context.Background(), &lobslawv1.AddCommitmentRequest{
		Commitment: &lobslawv1.AgentCommitment{
			DueAt:          timestamppb.New(time.Now().Add(time.Minute)),
			Reason:         "x",
			ClaimedBy:      "attacker-node",
			ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Hour)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := store.Get(memory.BucketCommitments, resp.Id)
	var stored lobslawv1.AgentCommitment
	_ = proto.Unmarshal(raw, &stored)
	if stored.ClaimedBy != "" {
		t.Errorf("caller-supplied ClaimedBy should be stripped; got %q", stored.ClaimedBy)
	}
	if stored.ClaimExpiresAt != nil {
		t.Errorf("caller-supplied ClaimExpiresAt should be stripped; got %v", stored.ClaimExpiresAt)
	}
}

func TestAddCommitmentRejectsEmpty(t *testing.T) {
	t.Parallel()
	svc, _, _ := newHarness(t)
	_, err := svc.AddCommitment(context.Background(), &lobslawv1.AddCommitmentRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("empty request should be InvalidArgument; got %v", err)
	}
}

func TestAddCommitmentRequiresDueAt(t *testing.T) {
	t.Parallel()
	svc, _, _ := newHarness(t)
	_, err := svc.AddCommitment(context.Background(), &lobslawv1.AddCommitmentRequest{
		Commitment: &lobslawv1.AgentCommitment{Reason: "x"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("missing due_at should be InvalidArgument; got %v", err)
	}
}

// --- CancelCommitment -----------------------------------------------------

func TestCancelCommitmentPendingToCancelled(t *testing.T) {
	t.Parallel()
	svc, node, store := newHarness(t)

	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id:     "c1",
		DueAt:  timestamppb.New(time.Now().Add(time.Hour)),
		Status: "pending",
	})

	if _, err := svc.CancelCommitment(context.Background(),
		&lobslawv1.CancelCommitmentRequest{Id: "c1"}); err != nil {
		t.Fatal(err)
	}

	raw, _ := store.Get(memory.BucketCommitments, "c1")
	var stored lobslawv1.AgentCommitment
	_ = proto.Unmarshal(raw, &stored)
	if stored.Status != "cancelled" {
		t.Errorf("status after cancel: %q", stored.Status)
	}
}

// TestCancelCommitmentInFlightReturnsAborted — if a handler is
// mid-firing (claim is held, not expired), cancel fails with
// Aborted. The caller can retry after the handler's completion
// clears the claim.
func TestCancelCommitmentInFlightReturnsAborted(t *testing.T) {
	t.Parallel()
	svc, node, _ := newHarness(t)

	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id:             "c1",
		DueAt:          timestamppb.New(time.Now().Add(time.Hour)),
		Status:         "pending",
		ClaimedBy:      "node-a",
		ClaimExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
	})

	_, err := svc.CancelCommitment(context.Background(),
		&lobslawv1.CancelCommitmentRequest{Id: "c1"})
	if status.Code(err) != codes.Aborted {
		t.Errorf("in-flight cancel should be Aborted; got %v", err)
	}
}

// TestCancelCommitmentStaleClaimSucceeds — a commitment with an
// EXPIRED claim (claimer crashed) can still be cancelled. The FSM's
// expiry bypass treats the stale claim as "unclaimed" so the
// expected="" CAS lands.
func TestCancelCommitmentStaleClaimSucceeds(t *testing.T) {
	t.Parallel()
	svc, node, store := newHarness(t)

	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id:             "c1",
		DueAt:          timestamppb.New(time.Now().Add(time.Hour)),
		Status:         "pending",
		ClaimedBy:      "crashed-node",
		ClaimExpiresAt: timestamppb.New(time.Now().Add(-time.Hour)), // already expired
	})

	if _, err := svc.CancelCommitment(context.Background(),
		&lobslawv1.CancelCommitmentRequest{Id: "c1"}); err != nil {
		t.Fatalf("stale-claim cancel should succeed; got %v", err)
	}
	raw, _ := store.Get(memory.BucketCommitments, "c1")
	var stored lobslawv1.AgentCommitment
	_ = proto.Unmarshal(raw, &stored)
	if stored.Status != "cancelled" {
		t.Errorf("status: %q", stored.Status)
	}
}

func TestCancelCommitmentNotFound(t *testing.T) {
	t.Parallel()
	svc, _, _ := newHarness(t)
	_, err := svc.CancelCommitment(context.Background(),
		&lobslawv1.CancelCommitmentRequest{Id: "does-not-exist"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("unknown id should be NotFound; got %v", err)
	}
}

func TestCancelCommitmentAlreadyDone(t *testing.T) {
	t.Parallel()
	svc, node, _ := newHarness(t)
	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id:     "c1",
		DueAt:  timestamppb.New(time.Now().Add(time.Hour)),
		Status: "done",
	})
	_, err := svc.CancelCommitment(context.Background(),
		&lobslawv1.CancelCommitmentRequest{Id: "c1"})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("already-done cancel should be FailedPrecondition; got %v", err)
	}
}

// --- GetPlan -------------------------------------------------------------

func TestGetPlanIncludesDueCommitments(t *testing.T) {
	t.Parallel()
	svc, node, _ := newHarness(t)

	now := time.Now()
	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "in-window", Status: "pending",
		DueAt: timestamppb.New(now.Add(time.Hour)),
	})
	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "out-of-window", Status: "pending",
		DueAt: timestamppb.New(now.Add(48 * time.Hour)),
	})
	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "done-already", Status: "done",
		DueAt: timestamppb.New(now.Add(time.Hour)),
	})

	resp, err := svc.GetPlan(context.Background(), &lobslawv1.GetPlanRequest{
		Window: durationpb.New(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Commitments) != 1 || resp.Commitments[0].Id != "in-window" {
		t.Errorf("expected only in-window pending; got %+v", resp.Commitments)
	}
}

func TestGetPlanIncludesDueScheduledTasks(t *testing.T) {
	t.Parallel()
	svc, node, _ := newHarness(t)

	now := time.Now()
	putTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t-soon", Enabled: true,
		NextRun: timestamppb.New(now.Add(30 * time.Minute)),
	})
	putTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t-far", Enabled: true,
		NextRun: timestamppb.New(now.Add(48 * time.Hour)),
	})
	putTask(t, node, &lobslawv1.ScheduledTaskRecord{
		Id: "t-disabled", Enabled: false,
		NextRun: timestamppb.New(now.Add(time.Hour)),
	})

	resp, err := svc.GetPlan(context.Background(), &lobslawv1.GetPlanRequest{
		Window: durationpb.New(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ScheduledTasks) != 1 || resp.ScheduledTasks[0].Id != "t-soon" {
		t.Errorf("expected only t-soon; got %+v", resp.ScheduledTasks)
	}
}

func TestGetPlanDefaultWindowIs24h(t *testing.T) {
	t.Parallel()
	svc, _, _ := newHarness(t)

	resp, err := svc.GetPlan(context.Background(), &lobslawv1.GetPlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Window.AsDuration() != 24*time.Hour {
		t.Errorf("default window: %v", resp.Window.AsDuration())
	}
}

// TestGetPlanOrdersByNextFireTime — commitments and tasks each come
// back sorted ascending so UIs can render them as a timeline without
// client-side work.
func TestGetPlanOrdersByNextFireTime(t *testing.T) {
	t.Parallel()
	svc, node, _ := newHarness(t)

	now := time.Now()
	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "later", Status: "pending",
		DueAt: timestamppb.New(now.Add(3 * time.Hour)),
	})
	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "sooner", Status: "pending",
		DueAt: timestamppb.New(now.Add(1 * time.Hour)),
	})
	putCommitment(t, node, &lobslawv1.AgentCommitment{
		Id: "middle", Status: "pending",
		DueAt: timestamppb.New(now.Add(2 * time.Hour)),
	})

	resp, err := svc.GetPlan(context.Background(), &lobslawv1.GetPlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sooner", "middle", "later"}
	if len(resp.Commitments) != len(want) {
		t.Fatalf("count: got %d want %d", len(resp.Commitments), len(want))
	}
	for i, w := range want {
		if resp.Commitments[i].Id != w {
			t.Errorf("order[%d]: got %q want %q", i, resp.Commitments[i].Id, w)
		}
	}
}

// TestGetPlanEmptyStore — no tasks, no commitments: empty-but-valid
// response with the default window set.
func TestGetPlanEmptyStore(t *testing.T) {
	t.Parallel()
	svc, _, _ := newHarness(t)
	resp, err := svc.GetPlan(context.Background(), &lobslawv1.GetPlanRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Commitments) != 0 || len(resp.ScheduledTasks) != 0 {
		t.Errorf("empty store should produce empty plan; got %+v", resp)
	}
}
