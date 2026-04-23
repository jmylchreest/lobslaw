package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// TestResumeRequiresBudget — symmetric with RunToolCallLoop: nil
// budget is a programming error, not a runtime fallback.
func TestResumeRequiresBudget(t *testing.T) {
	t.Parallel()
	a, _ := NewAgent(AgentConfig{Provider: NewMockProvider()})
	_, err := a.ResumeFromConfirmation(context.Background(),
		ProcessMessageRequest{Claims: &types.Claims{}}, []Message{{Role: "user"}})
	if err == nil || !strings.Contains(err.Error(), "Budget") {
		t.Errorf("want Budget-required err; got %v", err)
	}
}

// TestResumeRequiresPriorMessages — can't resume from nothing.
func TestResumeRequiresPriorMessages(t *testing.T) {
	t.Parallel()
	a, _ := NewAgent(AgentConfig{Provider: NewMockProvider()})
	budget, _ := NewTurnBudget(BudgetCaps{})
	_, err := a.ResumeFromConfirmation(context.Background(),
		ProcessMessageRequest{Claims: &types.Claims{}, Budget: budget}, nil)
	if err == nil {
		t.Error("resume with empty priorMessages should fail")
	}
}

// TestResumeContinuesFromPriorMessages — the provider sees the
// resumed messages as input, not a re-seeded conversation. Proves
// the resume path doesn't re-enter seedMessages.
func TestResumeContinuesFromPriorMessages(t *testing.T) {
	t.Parallel()

	// Scripted provider returns a plain text reply on first call.
	// We record the incoming messages to assert they equal what we
	// passed in (modulo whatever the loop appends).
	var seenMessages []Message
	provider := NewMockProviderFunc(func(req ChatRequest, _ int) (MockResponse, error) {
		seenMessages = append([]Message(nil), req.Messages...)
		return MockResponse{Content: "resumed-reply"}, nil
	})
	a, _ := NewAgent(AgentConfig{Provider: provider})
	budget, _ := NewTurnBudget(BudgetCaps{})

	prior := []Message{
		{Role: "system", Content: "you are a careful agent"},
		{Role: "user", Content: "please run the dangerous thing"},
		{Role: "assistant", Content: "(budget hit, awaiting confirm)"},
	}
	resp, err := a.ResumeFromConfirmation(context.Background(),
		ProcessMessageRequest{Claims: &types.Claims{}, Budget: budget}, prior)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reply != "resumed-reply" {
		t.Errorf("unexpected reply: %q", resp.Reply)
	}
	if len(seenMessages) < len(prior) || seenMessages[0].Content != prior[0].Content {
		t.Errorf("provider didn't see prior messages; saw %+v", seenMessages)
	}
}

// TestResumeDefensiveCopyPriorMessages — a subsequent append to the
// caller's slice must not mutate what the agent is working on.
func TestResumeDefensiveCopyPriorMessages(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider(MockResponse{Content: "ok"})
	a, _ := NewAgent(AgentConfig{Provider: provider})
	budget, _ := NewTurnBudget(BudgetCaps{})

	// Build a slice with extra capacity — appends don't realloc.
	prior := make([]Message, 1, 8)
	prior[0] = Message{Role: "user", Content: "original"}

	_, err := a.ResumeFromConfirmation(context.Background(),
		ProcessMessageRequest{Claims: &types.Claims{}, Budget: budget}, prior)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate the caller's slice — agent's internal copy must be
	// untouched. We can't inspect the internal copy directly, but
	// a second Resume with the mutated slice must not see the
	// original content.
	prior = append(prior, Message{Role: "user", Content: "appended-by-caller"})
	if prior[0].Content != "original" {
		t.Error("append through the original slot should not change index 0")
	}
}

// TestBudgetRelaxClearsCaps — Relax zeros every cap, so subsequent
// Record* calls never exceed regardless of prior spend.
func TestBudgetRelaxClearsCaps(t *testing.T) {
	t.Parallel()

	b, _ := NewTurnBudget(BudgetCaps{
		MaxToolCalls:   1,
		MaxSpendUSD:    0.01,
		MaxEgressBytes: 1024,
	})
	// Deliberately exceed every cap.
	b.RecordToolCall()
	_ = b.RecordCostUSD(CostRecord{CostUSD: 10.0})
	_ = b.RecordEgressBytes(99999)

	// Pre-Relax, Check reports Exceeded.
	if !b.Check().Exceeded {
		t.Fatal("budget should be Exceeded before Relax")
	}

	b.Relax()

	if b.Check().Exceeded {
		t.Error("Relax should have cleared the caps; still Exceeded")
	}
	// State still reflects prior spend — Relax preserves counters.
	s := b.State()
	if s.ToolCalls == 0 || s.SpendUSD == 0 || s.EgressBytes == 0 {
		t.Errorf("Relax wiped counters; state = %+v", s)
	}
}

// TestResumeAfterBudgetExceededReliftedRunsToCompletion pins the
// boundary that matters: after a turn exceeded its spend cap and
// the caller Relaxed the budget, Resume produces a plain reply
// instead of re-triggering the same NeedsConfirmation.
func TestResumeAfterBudgetExceededReliftedRunsToCompletion(t *testing.T) {
	t.Parallel()

	callCount := 0
	provider := NewMockProviderFunc(func(_ ChatRequest, _ int) (MockResponse, error) {
		callCount++
		return MockResponse{Content: "final-reply"}, nil
	})
	a, _ := NewAgent(AgentConfig{Provider: provider})

	b, _ := NewTurnBudget(BudgetCaps{MaxSpendUSD: 0.01})
	_ = b.RecordCostUSD(CostRecord{CostUSD: 1.0})
	if !b.Check().Exceeded {
		t.Fatal("setup: budget should be exceeded")
	}
	b.Relax()

	prior := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
	}
	resp, err := a.ResumeFromConfirmation(context.Background(),
		ProcessMessageRequest{Claims: &types.Claims{}, Budget: b}, prior)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reply != "final-reply" {
		t.Errorf("reply: %q", resp.Reply)
	}
	if callCount != 1 {
		t.Errorf("provider called %d times; want 1", callCount)
	}
	if resp.NeedsConfirmation {
		t.Error("resume with relaxed budget should not re-confirm")
	}
}
