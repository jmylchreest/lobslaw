package compute

import (
	"context"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestTaskGrantDoesNotBorrowConversationOrOtherTask(t *testing.T) {
	e, sessions := modeGatedExecutor(t, []string{"strict"})
	ctx := turn.WithIdentity(context.Background(), turn.Identity{Channel: "telegram", ChannelID: "42"})
	sessions.Grant(ctx, ShellAction, "(risk=reads)")
	check := func(_ context.Context, q *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
		return &pb.CheckGrantTaskApprovalResponse{Granted: q.Id == "parent" && q.Resource == "(risk=reads)"}, nil
	}
	// No turn identity is needed by this direct gate test; it tests grant scope.
	task := WithTaskExecution(context.Background(), turn.TaskScope{ID: "child", Owner: "user:alice", Actor: "user:alice", ClaimToken: "token"}, check)
	if err := checkShell(task, t, e, "pwd && ls"); !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("child bypassed approval: %v", err)
	}
	parent := WithTaskExecution(context.Background(), turn.TaskScope{ID: "parent", Owner: "user:alice", Actor: "user:alice", ClaimToken: "token"}, check)
	for _, cmd := range []string{"pwd && ls", "uname -a"} {
		if err := checkShell(parent, t, e, cmd); err != nil {
			t.Fatalf("variable read %q: %v", cmd, err)
		}
	}
	if err := checkShell(parent, t, e, "touch /tmp/a"); !errors.Is(err, ErrRequireConfirm) {
		t.Fatalf("read grant widened to write: %v", err)
	}
}
func TestTaskGrantCannotOverridePolicyDenialOrLostAuthority(t *testing.T) {
	deny := &pb.PolicyRule{Id: "deny-shell", Subject: "*", Action: ShellAction, Resource: "*", Effect: "deny", Priority: 100}
	e, _ := modeGatedExecutor(t, []string{"strict"}, deny)
	check := func(context.Context, *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
		return &pb.CheckGrantTaskApprovalResponse{Granted: true}, nil
	}
	ctx := WithTaskExecution(context.Background(), turn.TaskScope{ID: "task", Owner: "user:alice", Actor: "user:alice", ClaimToken: "token"}, check)
	if err := checkShell(ctx, t, e, "uname -a"); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("grant overrode deny: %v", err)
	}
	live, _ := modeGatedExecutor(t, []string{"standard"})
	lost := errors.New("task cancelled")
	ctx = WithTaskExecution(context.Background(), turn.TaskScope{ID: "task", Owner: "user:alice", Actor: "user:alice", ClaimToken: "token"}, func(context.Context, *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
		return nil, lost
	})
	if err := checkShell(ctx, t, live, "uname -a"); !errors.Is(err, lost) {
		t.Fatalf("normal policy allow bypassed task cancellation: %v", err)
	}
}
