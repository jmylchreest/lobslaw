package compute

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

type staticBotResolver struct{ p *BotProfile }

func (s staticBotResolver) ResolveBot(context.Context, string) (*BotProfile, error) {
	return s.p, nil
}

func TestResumeTightensBotCaps(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "ok"})
	a, err := NewAgent(AgentConfig{
		Provider: provider,
		Bots:     staticBotResolver{p: &BotProfile{ID: "eng", Caps: BudgetCaps{MaxToolCalls: 3}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	budget, err := NewTurnBudget(BudgetCaps{MaxToolCalls: 30})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.ResumeFromConfirmation(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{},
		Budget: budget,
		BotID:  "eng",
	}, []Message{{Role: "system", Content: "s"}, {Role: "user", Content: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	if budget.Caps().MaxToolCalls != 3 {
		t.Fatalf("resume left node caps %d; want bot cap 3", budget.Caps().MaxToolCalls)
	}
}
