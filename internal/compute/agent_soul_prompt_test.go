package compute

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/soul"
)

func TestResumeKeepsOriginalSoulPrompt(t *testing.T) {
	const original = "Original soul configuration; not a user question."
	provider := NewMockProviderFunc(func(req ChatRequest, _ int) (MockResponse, error) {
		if req.Messages[0].Role != "system" || req.Messages[0].Content != original {
			t.Fatal("resume changed the turn's system prompt")
		}
		return MockResponse{Content: "42", FinishReason: "stop"}, nil
	})
	a, err := NewAgent(AgentConfig{Provider: provider, SoulSnapshot: func(context.Context) (*soul.Soul, error) {
		return nil, errors.New("resume must not load a new soul")
	}})
	if err != nil {
		t.Fatal(err)
	}
	budget, _ := NewTurnBudget(BudgetCaps{})
	_, err = a.ResumeFromConfirmation(context.Background(), ProcessMessageRequest{Budget: budget}, []Message{
		{Role: "system", Content: original}, {Role: "user", Content: "What is seven times six?"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSoulSnapshotStaysInSystemPrompt(t *testing.T) {
	const question = "What is seven times six?"
	const guidance = "Use short sentences and dry humour."
	s := soul.DefaultSoul()
	s.Body = guidance
	var seen []ChatRequest
	provider := NewMockProviderFunc(func(req ChatRequest, _ int) (MockResponse, error) {
		seen = append(seen, req)
		return MockResponse{Content: "42", FinishReason: "stop"}, nil
	})
	reads := 0
	a, err := NewAgent(AgentConfig{Provider: provider, SoulSnapshot: func(context.Context) (*soul.Soul, error) {
		reads++
		return s, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		budget, _ := NewTurnBudget(BudgetCaps{})
		if _, err := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{Message: question, Budget: budget}); err != nil {
			t.Fatal(err)
		}
		s.Body = "Use complete sentences."
	}
	if reads != 2 {
		t.Fatalf("snapshot reads = %d; want one per turn", reads)
	}
	for i, req := range seen {
		var system, user string
		for _, m := range req.Messages {
			switch m.Role {
			case "system":
				system = m.Content
			case "user":
				user = m.Content
			}
		}
		if user != question {
			t.Fatalf("soul contaminated user message: %q", user)
		}
		want := guidance
		if i == 1 {
			want = s.Body
		}
		if !strings.Contains(system, want) || !strings.Contains(system, "not a question") || !strings.Contains(system, "Do not acknowledge") {
			t.Fatalf("soul or its configuration framing missing from turn %d", i)
		}
	}
}
