package turn

// Message is one turn in a conversation. Role + content match
// OpenAI's shape; ToolCalls / ToolCallID are populated for the
// tool-calling round-trip.
type Message struct {
	// Role is one of "system" | "user" | "assistant" | "tool".
	Role string

	// Content is the text of the message. For role="tool" this is
	// the tool's output, typically wrapped in untrusted delimiters
	// before being placed here.
	Content string

	// ToolCalls is populated on assistant messages that requested
	// one or more tool invocations.
	ToolCalls []ToolCall

	// ToolCallID is populated on tool-result messages (role="tool")
	// and correlates to the originating assistant ToolCall.ID.
	ToolCallID string
}

// ToolCall is the model's request to invoke a tool. ID is the
// round-trip correlation identifier (assistant says "call X with
// args"; the subsequent tool-role message carries ID matching).
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolInvocation records one tool call's lifecycle within a turn.
type ToolInvocation struct {
	CallID   string
	ToolName string
	Args     string
	Output   string
	ExitCode int
	Error    string
}
