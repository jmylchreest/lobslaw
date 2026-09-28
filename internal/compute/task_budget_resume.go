package compute

import "context"

func resumePendingInvocation(ctx context.Context, messages []Message) bool {
	if turnApprovalPending(ctx) {
		return true
	}
	if ctx.Value(approvedTaskBudgetKey{}) != true {
		return false
	}
	// Never infer non-execution from provider/tool text. Only the runner may
	// stamp this bit before dispatch, and it survives the durable codec.
	_, index, pending := pendingToolCall(messages)
	return pending && messages[index].BudgetPending
}

// A pause may interrupt an assistant batch. Results already present are never
// dispatched again; the untouched suffix must not disappear at the next LLM call.
func unansweredToolCalls(messages []Message, pendingIndex int) []ToolCall {
	for i := pendingIndex - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		answered := make(map[string]bool)
		for _, message := range messages[i+1:] {
			if message.Role == "tool" {
				answered[message.ToolCallID] = true
			}
		}
		var remaining []ToolCall
		for _, call := range messages[i].ToolCalls {
			if !answered[call.ID] {
				remaining = append(remaining, call)
			}
		}
		return remaining
	}
	return nil
}
