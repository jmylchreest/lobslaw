package node_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/node"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestTaskApprovalSurvivesNodeRestartOverMTLS(t *testing.T) {
	if testing.Short() {
		t.Skip("node restart integration")
	}
	dir := t.TempDir()
	creds := signNodeCert(t, filepath.Join(dir, "certs"), "task-node")
	cfg := node.Config{NodeID: "task-node", Functions: []types.NodeFunction{types.FunctionMemory, types.FunctionPolicy, types.FunctionStorage}, ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "data"), Bootstrap: true, SnapshotTarget: "storage:test-backup", Creds: creds, MemoryKey: mustKey(t)}
	boot := func() (pb.TaskApprovalServiceClient, func()) {
		n, e := node.New(cfg)
		if e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- n.Start(ctx) }()
		var once sync.Once
		stop := func() {
			once.Do(func() {
				cancel()
				if e := <-done; e != nil {
					t.Errorf("shutdown: %v", e)
				}
			})
		}
		t.Cleanup(stop)
		if e = n.Raft().WaitForLeader(5 * time.Second); e != nil {
			t.Fatal(e)
		}
		conn, e := grpc.NewClient(n.ListenAddr(), grpc.WithTransportCredentials(clientCreds(t, creds)))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return pb.NewTaskApprovalServiceClient(conn), stop
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, stop := boot()
	created, e := client.CreateTaskApproval(ctx, &pb.CreateTaskApprovalRequest{Owner: "user:alice", Actor: "user:alice"})
	if e != nil {
		t.Fatal(e)
	}
	r := created.Record
	paused, e := client.PauseTaskApproval(ctx, &pb.PauseTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: r.Revision, ClaimToken: r.ClaimedBy, TurnId: "task-turn", Operation: &pb.TaskOperation{RequiresBudgetExtension: true, Summary: "tool-call budget exhausted"}, BudgetPolicy: &pb.TaskBudget{ToolCalls: 3}, BudgetLimits: &pb.TaskBudget{ToolCalls: 3}, Continuation: &pb.Continuation{ToolCalls: 4, Messages: []*pb.SessionMessage{{Role: "assistant", Content: "saved research"}}}})
	if e != nil {
		t.Fatal(e)
	}
	stop()
	client, _ = boot()
	if _, e = client.GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: r.Id, Owner: "user:bob"}); status.Code(e) != codes.NotFound {
		t.Fatalf("another owner saw restart state: %v", e)
	}
	got, e := client.GetTaskApproval(ctx, &pb.GetTaskApprovalRequest{Id: r.Id, Owner: r.Owner})
	if e != nil {
		t.Fatal(e)
	}
	if got.Record.State != pb.TaskApprovalState_TASK_APPROVAL_STATE_WAITING || got.Record.Continuation != nil {
		t.Fatal("restart lost wait or leaked transcript")
	}
	approved, e := client.DecideTaskApproval(ctx, &pb.DecideTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Revision: paused.Record.Revision, Choice: pb.TaskApprovalChoice_TASK_APPROVAL_CHOICE_BUDGET_EXTENSION, ExtraBudget: &pb.TaskBudget{ToolCalls: 2}})
	if e != nil {
		t.Fatal(e)
	}
	claimed, e := client.ClaimTaskApproval(ctx, &pb.ClaimTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: approved.Record.Revision})
	if e != nil {
		t.Fatal(e)
	}
	if claimed.Record.Continuation.Messages[0].Content != "saved research" || claimed.Record.BudgetLimits.ToolCalls != 6 {
		t.Fatal("checkpoint or bounded allowance changed")
	}
	_, e = client.FinishTaskApproval(ctx, &pb.FinishTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, Revision: claimed.Record.Revision, ClaimToken: claimed.Record.ClaimedBy, Result: "done"})
	if e != nil {
		t.Fatal(e)
	}
	_, e = client.CheckGrantTaskApproval(ctx, &pb.CheckGrantTaskApprovalRequest{Id: r.Id, Owner: r.Owner, Actor: r.Actor, ClaimToken: claimed.Record.ClaimedBy})
	if status.Code(e) != codes.FailedPrecondition {
		t.Fatal("completed task retained execution authority")
	}
}
