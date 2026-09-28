package compute

import "testing"

func TestEmptyAllowlistWithoutAskBotDoesNotRegrantIt(t *testing.T) {
	t.Parallel()
	all := []Tool{{Name: "ask_bot"}, {Name: "read_file"}, {Name: "inbox_post"}}
	child := (&BotProfile{}).Without("ask_bot")
	got := child.FilterTools(all)
	for _, tdef := range got {
		if tdef.Name == "ask_bot" {
			t.Fatal("empty allowlist must not re-grant ask_bot after Without")
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d tools, want 2", len(got))
	}
}

func TestTightenNeverWidens(t *testing.T) {
	t.Parallel()
	b, err := NewTurnBudget(BudgetCaps{MaxToolCalls: 10, MaxSpendUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	b.Tighten(BudgetCaps{MaxToolCalls: 5})
	if b.Caps().MaxToolCalls != 5 {
		t.Fatalf("tighten down: %d", b.Caps().MaxToolCalls)
	}
	b.Tighten(BudgetCaps{MaxToolCalls: 500, MaxSpendUSD: 99})
	if b.Caps().MaxToolCalls != 5 || b.Caps().MaxSpendUSD != 1 {
		t.Fatalf("widen attempted: %+v", b.Caps())
	}
}
