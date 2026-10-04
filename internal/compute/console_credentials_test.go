package compute

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestConsoleCredentialToolNeverExposedOrDispatched(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"direct human", "specialist", "coordinator task", "human task"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			req := ProcessMessageRequest{Principal: identity.User("alice"), Claims: &types.Claims{UserID: "alice", Roles: []string{identity.RoleOperator}}, Budget: mkBudget(t, BudgetCaps{}), Tools: []Tool{{Name: retiredConsoleCodeTool}}}
			if name == "specialist" || name == "coordinator task" {
				req.BotID = "worker"
				req.Principal = identity.Bot("worker")
				req.Bot = &BotProfile{ID: "worker", Owner: "user:alice", IsCoordinator: name == "coordinator task", Tools: []string{retiredConsoleCodeTool}}
			}
			if name != "direct human" {
				ctx = WithTaskExecution(ctx, turn.TaskScope{ID: "task", Owner: "user:alice", Actor: req.Principal.String(), ClaimToken: "claim", CoordinatorConversation: name == "coordinator task"}, func(context.Context, *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
					return &pb.CheckGrantTaskApprovalResponse{Granted: true}, nil
				})
			}
			dispatcher := &fakeSkillDispatcher{known: map[string]struct{}{retiredConsoleCodeTool: {}}, response: &SkillInvokeResult{Stdout: []byte("credential")}}
			a := &Agent{cfg: AgentConfig{Skills: dispatcher}}
			if len(visibleTaskTools(ctx, req)) != 0 {
				t.Fatal("credential tool exposed to model")
			}
			if len(a.visibleSkills(ctx, req, []promptgen.SkillInfo{{Name: retiredConsoleCodeTool}})) != 0 {
				t.Fatal("credential tool exposed through skill index")
			}
			inv, _, err := a.runToolCall(ctx, req, ToolCall{Name: retiredConsoleCodeTool, Arguments: `{}`})
			if err != nil || inv.Error == "" || dispatcher.calls != 0 {
				t.Fatalf("credential tool dispatched: %+v, %v, calls=%d", inv, err, dispatcher.calls)
			}
		})
	}
}
