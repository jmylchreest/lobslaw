package compute

import "encoding/json"

// The shape every builtin failure takes, and the helpers the guard
// chain shares with them.
//
// These lived in builtin_fs.go, which made them look like a
// filesystem concern. They are not: marshalToolError is what EVERY
// builtin returns a failure as, and hardline.go, mount_resolver.go
// and path_guard.go — none of them a tool — all call it. A helper
// declared inside one tool and used by the infrastructure around all
// of them is filed under the first caller that needed it rather than
// under what it is.

// toolError is the structured failure shape fs/exec builtins emit
// on error. Mirrors opencode's pattern: every failure carries a
// category + human message + actionable next step the LLM can
// follow. Returning this as stdout JSON (exit code 0, but with
// error_type set) keeps the LLM's tool-call result parseable as
// JSON every time — saves it from regex-splitting stderr.
type toolError struct {
	ErrorType  string `json:"error_type"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

// marshalToolError encodes the structured error. Returns exitCode=1
// so the executor still treats it as a tool failure, but the JSON
// body carries actionable detail. Use instead of fmt.Errorf in the
// builtins that have something useful to say about a failure.
func marshalToolError(errType, msg, suggestion string) ([]byte, int, error) {
	payload, err := json.Marshal(toolError{ErrorType: errType, Message: msg, Suggestion: suggestion})
	if err != nil {
		return nil, 1, err
	}
	return payload, 1, nil
}

// firstNonEmpty returns a, or b when a is blank.
//
// Declared in builtin_council.go and used by image.go and speak.go,
// which is two subsystems reaching into a tool for a two-line string
// helper. It belongs with neither.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
