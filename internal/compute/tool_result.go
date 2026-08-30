package compute

import "encoding/json"

// The shape every builtin failure takes.
//
// Shared by the tools and by the guard chain around them —
// hardline.go, mount_resolver.go and path_guard.go all return
// failures this way, and none of them is a tool.

// ToolError is the structured failure shape fs/exec builtins emit
// on error. Mirrors opencode's pattern: every failure carries a
// category + human message + actionable next step the LLM can
// follow. Returning this as stdout JSON (exit code 0, but with
// error_type set) keeps the LLM's tool-call result parseable as
// JSON every time — saves it from regex-splitting stderr.
type ToolError struct {
	ErrorType  string `json:"error_type"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

// MarshalToolError encodes the structured error. Returns exitCode=1
// so the executor still treats it as a tool failure, but the JSON
// body carries actionable detail. Use instead of fmt.Errorf in the
// builtins that have something useful to say about a failure.
func MarshalToolError(errType, msg, suggestion string) ([]byte, int, error) {
	payload, err := json.Marshal(ToolError{ErrorType: errType, Message: msg, Suggestion: suggestion})
	if err != nil {
		return nil, 1, err
	}
	return payload, 1, nil
}
