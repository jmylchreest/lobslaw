package compute

import (
	"testing"
)

// An ambiguous tool-call id must not resolve to a guess. Ids come off
// the wire verbatim and some OpenAI-compatible servers repeat or omit
// them; re-running "whichever matched first" would execute something
// the user was never asked about.
func TestAnAmbiguousToolCallIsNotReExecuted(t *testing.T) {
	t.Parallel()
	dup := []Message{
		{Role: "user", Content: "do two things"},
		{Role: "assistant", ToolCalls: []ToolCall{
			{ID: "same", Name: "memory_write", Arguments: `{}`},
			{ID: "same", Name: "shell_command", Arguments: `{"command":"echo x"}`},
		}},
		{Role: "tool", ToolCallID: "same", Content: ErrRequireConfirm.Error()},
	}
	if _, _, ok := pendingToolCall(dup); ok {
		t.Error("a duplicated tool-call id resolved to a guess")
	}

	empty := []Message{
		{Role: "user", Content: "do a thing"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "", Name: "shell_command", Arguments: `{}`}}},
		{Role: "tool", ToolCallID: "", Content: ErrRequireConfirm.Error()},
	}
	if _, _, ok := pendingToolCall(empty); ok {
		t.Error("an empty tool-call id resolved to a guess")
	}
}
