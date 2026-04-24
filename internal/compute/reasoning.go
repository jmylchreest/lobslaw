package compute

import (
	"regexp"
	"strings"
)

// reasoningTagRe matches common chain-of-thought wrappers models
// emit. MiniMax-M2 uses <think>…</think>; DeepSeek-R1 + QwQ use
// the same. Claude uses <thinking>…</thinking> when the feature
// is enabled. The regex is multiline and non-greedy so nested or
// multiple blocks strip correctly.
var reasoningTagRe = regexp.MustCompile(`(?is)<(think|thinking)>.*?</(think|thinking)>`)

// stripReasoningTags removes reasoning-model chain-of-thought
// wrappers from a user-facing reply. Whitespace around the
// removed blocks is collapsed so the reply doesn't end up with
// trailing newlines where thinking used to live. Idempotent — a
// reply with no tags passes through unchanged.
func stripReasoningTags(s string) string {
	cleaned := reasoningTagRe.ReplaceAllString(s, "")
	// Collapse runs of 3+ newlines (thinking block removal often
	// leaves gaps) and trim surrounding whitespace.
	cleaned = multiNewlineRe.ReplaceAllString(cleaned, "\n\n")
	return strings.TrimSpace(cleaned)
}

var multiNewlineRe = regexp.MustCompile(`\n{3,}`)
