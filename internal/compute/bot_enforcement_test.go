package compute

import (
	"context"
	"strings"
	"testing"
)

func TestBotDispatchRejectsExcludedTool(t *testing.T) {
	t.Parallel()
	b, err := NewTurnBudget(BudgetCaps{})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &fakeSkillDispatcher{known: map[string]struct{}{"read_file": {}}, response: &SkillInvokeResult{Stdout: []byte("private")}}
	a := &Agent{cfg: AgentConfig{Skills: dispatcher}}
	inv, _, err := a.runToolCall(context.Background(), ProcessMessageRequest{Budget: b, Bot: &BotProfile{Tools: []string{"memory_search"}}}, ToolCall{Name: "read_file", Arguments: `{}`})
	if err != nil || !strings.Contains(inv.Error, "bot tool restrictions") {
		t.Fatalf("excluded dispatch: %+v %v", inv, err)
	}
	if dispatcher.calls != 0 {
		t.Fatal("excluded skill executed")
	}
}

func TestNamedBotWithoutResolverFailsClosed(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	if err := a.resolveBot(context.Background(), &ProcessMessageRequest{BotID: "worker"}); err == nil {
		t.Fatal("named bot ran without resolver")
	}
}

func TestChildBudgetDoesNotTightenParent(t *testing.T) {
	t.Parallel()
	parent, _ := NewTurnBudget(BudgetCaps{MaxToolCalls: 10})
	for range 4 {
		parent.RecordToolCall()
	}
	child, err := parent.Child(BudgetCaps{MaxToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	if child.RecordToolCall().Exceeded {
		t.Fatal("parent's earlier spend charged against child allowance")
	}
	if parent.Caps().MaxToolCalls != 10 || parent.State().ToolCalls != 5 {
		t.Fatal("child did not preserve caps and shared accounting")
	}
	child.RecordToolCall()
	if !child.RecordToolCall().Exceeded {
		t.Fatal("child cap not enforced")
	}
	if parent.Check().Exceeded {
		t.Fatal("child cap changed parent")
	}
}

func TestChildEgressStopsFurtherToolExecution(t *testing.T) {
	t.Parallel()
	parent, _ := NewTurnBudget(BudgetCaps{MaxEgressBytes: 10})
	child, _ := parent.Child(BudgetCaps{})
	child.RecordEgressBytes(11)
	dispatcher := &fakeSkillDispatcher{known: map[string]struct{}{"read_file": {}}, response: &SkillInvokeResult{}}
	a := &Agent{cfg: AgentConfig{Skills: dispatcher}}
	_, confirmation, err := a.runToolCall(context.Background(), ProcessMessageRequest{Budget: child}, ToolCall{Name: "read_file", Arguments: `{}`})
	if err != nil || confirmation == nil || dispatcher.calls != 0 {
		t.Fatal("exhausted parent egress allowance allowed a child tool")
	}
}
