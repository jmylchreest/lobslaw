package turn

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
)

func TestContinuationPreservesPreparedInputAndBotIdentity(t *testing.T) {
	t.Parallel()
	const original = `{"command":"original"}`
	prepared := &PreparedToolCall{
		CallID: "call", ToolName: "shell_command", TurnID: "turn",
		OriginalArguments: original, Params: map[string]string{"command": "rewritten"},
		Approvals: []PreparedApproval{{Action: "shell_command", Resource: "rewritten"}},
	}
	in := &Continuation{
		Request: Request{BotID: "specialist", Principal: identity.User("alice"), Spent: BudgetState{ToolCalls: 3}},
		Messages: []Message{
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Name: "shell_command", Arguments: original}}},
			{Role: "tool", ToolCallID: "call", PreparedToolCall: prepared},
		},
	}
	wire := EncodeContinuation(in)
	if wire.Messages[0].ToolCalls[0].Arguments == original {
		t.Fatal("legacy wire arguments lost the prepared input")
	}
	out, err := DecodeContinuation(wire, BudgetCaps{MaxToolCalls: 5})
	if err != nil {
		t.Fatal(err)
	}
	if out.Request.BotID != in.Request.BotID || out.Request.Principal != in.Request.Principal || out.Request.Spent != in.Request.Spent {
		t.Fatalf("identity or consumption lost: %+v", out.Request)
	}
	if out.Messages[0].ToolCalls[0].Arguments != original || out.Messages[1].PreparedToolCall.Params["command"] != "rewritten" || len(out.Messages[1].PreparedToolCall.Approvals) != 1 {
		t.Fatalf("prepared binding lost: %+v", out.Messages)
	}
	wire.Messages[1].PreparedToolCall.Params["command"] = "mutated"
	if prepared.Params["command"] != "rewritten" || out.Messages[1].PreparedToolCall.Params["command"] != "rewritten" {
		t.Fatal("codec aliases trusted prepared parameters")
	}
}
