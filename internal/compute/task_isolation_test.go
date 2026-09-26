package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestTaskUsesSuppliedContextWithoutPinnedOrMemoryTools(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{ToolCalls: []ToolCall{{ID: "read", Name: "memory_search", Arguments: `{}`}}}, MockResponse{Content: "done"})
	pinnedReads := 0
	a, err := NewAgent(AgentConfig{Provider: provider, Soul: func() *types.SoulConfig { return &types.SoulConfig{} }, PinnedProvider: func(string, string) promptgen.PinnedBlocks { pinnedReads++; return promptgen.PinnedBlocks{} }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithTaskExecution(context.Background(), turn.TaskScope{ID: "task", Owner: "user:alice", Actor: "bot:worker", ClaimToken: "claim"}, func(context.Context, *pb.CheckGrantTaskApprovalRequest) (*pb.CheckGrantTaskApprovalResponse, error) {
		return &pb.CheckGrantTaskApprovalResponse{}, nil
	})
	response, err := a.RunToolCallLoop(ctx, ProcessMessageRequest{BotID: "worker", Bot: &BotProfile{ID: "worker", Owner: "user:alice"}, Principal: "bot:worker", Claims: &types.Claims{UserID: "alice"}, Budget: mkBudget(t, BudgetCaps{}), Message: "supplied context", Tools: []Tool{{Name: "memory_search"}}})
	if err != nil {
		t.Fatal(err)
	}
	if pinnedReads != 0 || len(response.ToolCalls) != 1 || !strings.Contains(response.ToolCalls[0].Error, "isolated") {
		t.Fatalf("task reached saved context: pins=%d response=%+v", pinnedReads, response)
	}
}

func TestSkillIndexAndDispatchShareBotRestrictions(t *testing.T) {
	t.Parallel()
	skills := &fakeSkillDispatcher{known: map[string]struct{}{"private-skill": {}}}
	a, err := NewAgent(AgentConfig{Provider: NewMockProvider(), Skills: skills, Soul: func() *types.SoulConfig { return &types.SoulConfig{} }, SkillsProvider: func() []promptgen.SkillInfo {
		return []promptgen.SkillInfo{{Name: "private-skill", Description: "private procedure"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	req := ProcessMessageRequest{Bot: &BotProfile{Tools: []string{"skill_view"}}, Budget: mkBudget(t, BudgetCaps{})}
	if err := a.fillDefaults(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.SystemPrompt, "private-skill") {
		t.Fatal("excluded skill leaked into index")
	}
	for _, call := range []ToolCall{{Name: "private-skill", Arguments: `{}`}, {Name: "skill_view", Arguments: `{"name":"private-skill"}`}} {
		inv, _, err := a.runToolCall(context.Background(), req, call)
		if err != nil || inv.Error == "" {
			t.Fatalf("excluded skill reachable: %+v %v", inv, err)
		}
	}
	if skills.calls != 0 {
		t.Fatal("excluded skill executed")
	}
}
