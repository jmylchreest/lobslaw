package compute

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Context budget defaults. Both are deliberately ON by default: the
// prior behaviour — replay every retained message verbatim, including
// tool results capped at Executor.MaxOutputBytes (10 MB) — makes the
// cost of a conversation quadratic in its length, because every turn
// re-sends every previous turn.
const (
	// DefaultTailTokens is the verbatim-history budget per turn.
	// ~4k leaves room for the system prompt, tools, and the model's
	// reply inside a 32k window while still covering several
	// multi-tool exchanges.
	DefaultTailTokens = 4000
	// DefaultHistoryToolResultBytes is how much of a REPLAYED tool
	// result survives. The turn that produced it always sees it in
	// full; on later turns the model needs to know what ran and
	// roughly what came back, not the other 9.9 MB.
	DefaultHistoryToolResultBytes = 512
)

// ContextBudget bounds how much prior conversation is replayed into a
// turn. Applied to history only — never to the current user message
// or to tool results produced during the turn in flight.
//
// It is a budget, not a limit: the numbers are estimates (see
// estimateTokens) and the provider enforces the real context window.
// Being approximately right cheaply beats being exactly right by
// vendoring a tokenizer per model family.
type ContextBudget struct {
	// TailTokens caps the estimated tokens of replayed history.
	// Oldest messages are dropped first. 0 disables trimming.
	TailTokens int

	// HistoryToolResultBytes truncates replayed tool results to this
	// many bytes, with a marker naming what was dropped. 0 disables
	// elision.
	HistoryToolResultBytes int
}

// DefaultContextBudget returns the budget applied when config leaves
// the section out entirely.
func DefaultContextBudget() ContextBudget {
	return ContextBudget{
		TailTokens:             DefaultTailTokens,
		HistoryToolResultBytes: DefaultHistoryToolResultBytes,
	}
}

// Apply returns the slice of history that fits the budget: tool
// results elided first (cheap, lossless for intent), then whole
// messages dropped from the oldest end until the estimate fits.
//
// Elision runs before trimming on purpose. Shrinking a 4 KB tool
// result to 512 bytes often saves enough that no message has to be
// dropped at all — keeping the shape of the conversation intact is
// worth more to the model than the bytes it lost.
//
// The input slice is never mutated.
func (b ContextBudget) Apply(history []Message) []Message {
	if len(history) == 0 {
		return history
	}
	out := history
	if b.HistoryToolResultBytes > 0 {
		out = elideToolResults(out, b.HistoryToolResultBytes)
	}
	if b.TailTokens > 0 {
		out = trimToTokens(out, b.TailTokens)
	}
	return dropOrphanedToolResults(out)
}

// elideToolResults copies history, truncating oversized tool-result
// bodies. Non-tool messages pass through untouched — user and
// assistant text is the cheap part and the part the model reasons
// over.
func elideToolResults(history []Message, maxBytes int) []Message {
	var out []Message
	copied := false
	for i, m := range history {
		if m.Role != "tool" || len(m.Content) <= maxBytes {
			continue
		}
		if !copied {
			out = make([]Message, len(history))
			copy(out, history)
			copied = true
		}
		kept := truncateAtRune(m.Content, maxBytes)
		dropped := len(m.Content) - len(kept)
		out[i].Content = kept +
			fmt.Sprintf("\n… [%d bytes elided from replayed tool output]", dropped)
	}
	if !copied {
		return history
	}
	return out
}

// trimToTokens keeps the newest messages that fit the budget. Walks
// backwards from the most recent message because recency is what
// makes "what did I just say" work; the older context is what the
// summary and semantic recall tiers are for.
//
// A single message larger than the whole budget is still kept when
// it would otherwise leave nothing — an over-budget turn beats an
// empty one, and the provider's own limit is the real backstop.
func trimToTokens(history []Message, budget int) []Message {
	total := 0
	cut := 0
	for i := len(history) - 1; i >= 0; i-- {
		total += estimateTokens(history[i])
		if total > budget {
			cut = i + 1
			break
		}
	}
	if cut == 0 {
		return history
	}
	if cut >= len(history) {
		return history[len(history)-1:]
	}
	return history[cut:]
}

// dropOrphanedToolResults removes tool-result messages whose
// originating assistant tool call is no longer in the window.
//
// This is not cosmetic. OpenAI-compatible providers reject a message
// with a tool_call_id that no preceding assistant message claims —
// so a trim that cuts an assistant message but keeps its results
// turns every subsequent turn into a 400. Trimming from the oldest
// end makes exactly that shape, every time it lands mid-turn.
func dropOrphanedToolResults(history []Message) []Message {
	seen := make(map[string]bool)
	var out []Message
	dropped := false
	for i, m := range history {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.Role == "tool" && !seen[m.ToolCallID] {
			if !dropped {
				// First orphan: everything before it was fine, so
				// take it as the starting point and copy from here.
				out = make([]Message, i, len(history))
				copy(out, history[:i])
				dropped = true
			}
			continue
		}
		if dropped {
			out = append(out, m)
		}
	}
	if !dropped {
		return history
	}
	return out
}

// truncateAtRune clips s to at most n bytes, backing off to the last
// character boundary rather than cutting one in half.
//
// Every byte budget in this package is a rough bound, so losing up to
// three more bytes costs nothing — while splitting a multi-byte
// character produces U+FFFD in the middle of text the model then reads
// back as conversation. Non-Latin scripts are expected traffic here:
// the soul layer ships a ten-language detector.
func truncateAtRune(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// estimateTokens approximates a message's cost without a tokenizer.
//
// Four bytes per token is the usual English rule of thumb; the
// per-message constant covers role framing and delimiters, which
// dominate for short messages. Every model family tokenizes
// differently, so exactness here would be false precision — the
// budget just needs to be in the right order of magnitude.
func estimateTokens(m Message) int {
	const perMessageOverhead = 4
	n := len(m.Content) / estimatedBytesPerToken
	for _, tc := range m.ToolCalls {
		n += (len(tc.Name) + len(tc.Arguments)) / estimatedBytesPerToken
		n += perMessageOverhead
	}
	if m.ToolCallID != "" {
		n += perMessageOverhead
	}
	return n + perMessageOverhead
}

// String renders the budget for logs.
func (b ContextBudget) String() string {
	var parts []string
	if b.TailTokens > 0 {
		parts = append(parts, fmt.Sprintf("tail=%dtok", b.TailTokens))
	} else {
		parts = append(parts, "tail=unbounded")
	}
	if b.HistoryToolResultBytes > 0 {
		parts = append(parts, fmt.Sprintf("tool_result=%dB", b.HistoryToolResultBytes))
	} else {
		parts = append(parts, "tool_result=full")
	}
	return strings.Join(parts, " ")
}
