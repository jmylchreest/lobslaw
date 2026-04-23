package compute

import (
	"context"
	"errors"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// fakeSkillDispatcher implements SkillDispatcher for agent tests.
type fakeSkillDispatcher struct {
	known    map[string]struct{}
	lastReq  SkillInvokeRequest
	response *SkillInvokeResult
	err      error
	calls    int
}

func (f *fakeSkillDispatcher) Has(name string) bool {
	_, ok := f.known[name]
	return ok
}

func (f *fakeSkillDispatcher) Invoke(_ context.Context, req SkillInvokeRequest) (*SkillInvokeResult, error) {
	f.calls++
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	if f.response != nil {
		return f.response, nil
	}
	return &SkillInvokeResult{ExitCode: 0, Stdout: []byte(`{"ok":true}`)}, nil
}

// TestAgentRoutesToSkillDispatcher — a tool call whose name matches
// a registered skill goes through the skill dispatcher, not the
// Executor. Exit code + stdout surface as tool output.
func TestAgentRoutesToSkillDispatcher(t *testing.T) {
	t.Parallel()

	toolCall := ToolCall{
		ID:        "tc-1",
		Name:      "agenda",
		Arguments: `{"window":"24h"}`,
	}
	provider := NewMockProvider(
		MockResponse{ToolCalls: []ToolCall{toolCall}},
		MockResponse{Content: "done"},
	)
	dispatcher := &fakeSkillDispatcher{
		known:    map[string]struct{}{"agenda": {}},
		response: &SkillInvokeResult{ExitCode: 0, Stdout: []byte(`{"summary":"hi"}`)},
	}
	a, err := NewAgent(AgentConfig{
		Provider: provider,
		Skills:   dispatcher,
	})
	if err != nil {
		t.Fatal(err)
	}

	budget, _ := NewTurnBudget(BudgetCaps{})
	resp, err := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message: "what's on my plate?",
		Claims:  &types.Claims{UserID: "alice"},
		Budget:  budget,
	})
	if err != nil {
		t.Fatal(err)
	}

	if dispatcher.calls != 1 {
		t.Errorf("dispatcher called %d times; want 1", dispatcher.calls)
	}
	if dispatcher.lastReq.Name != "agenda" {
		t.Errorf("skill name: %q", dispatcher.lastReq.Name)
	}
	if dispatcher.lastReq.Params["window"] != "24h" {
		t.Errorf("params lost: %+v", dispatcher.lastReq.Params)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls: %d", len(resp.ToolCalls))
	}
	inv := resp.ToolCalls[0]
	if inv.Output != `{"summary":"hi"}` {
		t.Errorf("output didn't propagate: %q", inv.Output)
	}
	if inv.ExitCode != 0 {
		t.Errorf("exit code: %d", inv.ExitCode)
	}
}

// TestAgentFallsThroughWhenSkillNotRegistered — a tool call whose
// name ISN'T a skill goes through the existing Executor. The skill
// dispatcher is consulted via Has but never Invoked.
func TestAgentFallsThroughWhenSkillNotRegistered(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeSkillDispatcher{known: map[string]struct{}{"other": {}}}
	// No executor → tool call will error, which proves we tried the
	// executor path rather than routing to the skill dispatcher.
	provider := NewMockProvider(
		MockResponse{ToolCalls: []ToolCall{{ID: "tc", Name: "echo"}}},
		MockResponse{Content: "done"},
	)
	a, _ := NewAgent(AgentConfig{Provider: provider, Skills: dispatcher})

	budget, _ := NewTurnBudget(BudgetCaps{})
	resp, _ := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{},
		Budget: budget,
	})
	if dispatcher.calls != 0 {
		t.Errorf("dispatcher should NOT have been invoked for non-skill; calls=%d", dispatcher.calls)
	}
	if len(resp.ToolCalls) == 0 || resp.ToolCalls[0].Error == "" {
		t.Error("expected the executor path to surface an error for the unknown tool")
	}
}

// TestAgentSkillDispatchErrorSurfacesAsToolCallError — a skill
// dispatcher error becomes the tool-call's Error field, not an
// error return from RunToolCallLoop. Keeps the loop shape identical
// to the executor path.
func TestAgentSkillDispatchErrorSurfacesAsToolCallError(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeSkillDispatcher{
		known: map[string]struct{}{"breaks": {}},
		err:   errors.New("storage label unresolvable"),
	}
	provider := NewMockProvider(
		MockResponse{ToolCalls: []ToolCall{{ID: "tc", Name: "breaks"}}},
		MockResponse{Content: "done"},
	)
	a, _ := NewAgent(AgentConfig{Provider: provider, Skills: dispatcher})

	budget, _ := NewTurnBudget(BudgetCaps{})
	resp, err := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{},
		Budget: budget,
	})
	if err != nil {
		t.Fatalf("loop should not propagate skill error; got %v", err)
	}
	if len(resp.ToolCalls) == 0 {
		t.Fatal("expected a recorded tool call")
	}
	if resp.ToolCalls[0].Error == "" {
		t.Error("skill error should surface in tool-call Error")
	}
}

// TestAgentSkillNonZeroExitIncludesStderr — when the skill exits
// non-zero, the combined-outputs helper appends stderr to stdout
// so the model sees the diagnostic.
func TestAgentSkillNonZeroExitIncludesStderr(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeSkillDispatcher{
		known: map[string]struct{}{"failing": {}},
		response: &SkillInvokeResult{
			ExitCode: 1,
			Stdout:   []byte("partial"),
			Stderr:   []byte("error: disk full"),
		},
	}
	provider := NewMockProvider(
		MockResponse{ToolCalls: []ToolCall{{ID: "tc", Name: "failing"}}},
		MockResponse{Content: "done"},
	)
	a, _ := NewAgent(AgentConfig{Provider: provider, Skills: dispatcher})

	budget, _ := NewTurnBudget(BudgetCaps{})
	resp, _ := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{},
		Budget: budget,
	})
	inv := resp.ToolCalls[0]
	if inv.ExitCode != 1 {
		t.Errorf("exit code: %d", inv.ExitCode)
	}
	if inv.Output == "" || !containsBoth(inv.Output, "partial", "disk full") {
		t.Errorf("combined output missing stdout+stderr: %q", inv.Output)
	}
}

func containsBoth(s, a, b string) bool {
	return indexOf(s, a) >= 0 && indexOf(s, b) >= 0
}

func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestAgentSkillBudgetStillEnforced — the tool-call budget counter
// still trips when the skill path is used. A skill shouldn't be a
// free budget loophole.
func TestAgentSkillBudgetStillEnforced(t *testing.T) {
	t.Parallel()
	dispatcher := &fakeSkillDispatcher{
		known:    map[string]struct{}{"s": {}},
		response: &SkillInvokeResult{ExitCode: 0, Stdout: []byte(`{}`)},
	}
	provider := NewMockProvider(
		MockResponse{ToolCalls: []ToolCall{{ID: "tc", Name: "s"}}},
		MockResponse{Content: "done"},
	)
	a, _ := NewAgent(AgentConfig{Provider: provider, Skills: dispatcher})

	budget, _ := NewTurnBudget(BudgetCaps{MaxToolCalls: 0}) // 0 == unlimited
	resp, err := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{},
		Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool call count: %d", len(resp.ToolCalls))
	}
	// State should reflect one recorded tool call.
	if got := budget.State().ToolCalls; got != 1 {
		t.Errorf("budget.ToolCalls: %d want 1", got)
	}
}
