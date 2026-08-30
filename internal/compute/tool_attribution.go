package compute

import (
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/trace"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// What a tool actually costs.
//
// The obvious answer is wrong. A tool's cost is not the call that ran
// it: it is the tokens its output contributes to every SUBSEQUENT
// prompt in the turn. A tool returning 8k tokens on the first of six
// model calls is not an 8k-token event — its output sits in the
// message list for the remaining five prompts, so it is billed five
// more times, at the prompt rate.
//
// In an agentic turn that is usually the dominant cost, and nothing
// surfaces it. An operator reading a bill sees "prompt tokens: large"
// with no way to attribute it to the tool that produced them.
//
// Both inputs are available at the moment the prompt is assembled —
// the agent builds the message list, so it knows exactly which tool
// results are in it. This is bookkeeping, not estimation.
//
// Except for one part that IS estimation, and is labelled as such:
// the token size of a tool result. Providers report usage for the
// prompt as a whole, never per message, so the share belonging to one
// tool result has to be approximated from its text. estimateTokens is
// the same chars/4 heuristic the context budget already uses — being
// consistently wrong with the budget is worth more than being
// differently wrong from it.

// toolEntry is one tool result and when it entered the message list.
type toolEntry struct {
	spanID       string
	name         string
	resultBytes  int
	resultTokens int
	// afterCall is the 1-based index of the LLM call whose tool_calls
	// produced this result. The result is present in the prompt of
	// every call after it.
	afterCall int
}

// toolAttributor accumulates a turn's tool results and, at the end,
// charges each one for the prompts it was carried in.
//
// Per turn and per goroutine in practice — the agent loop is
// sequential — but guarded anyway, because a future parallel tool
// dispatch would otherwise corrupt the counts silently rather than
// tripping the race detector in CI.
type toolAttributor struct {
	rec     *trace.Recorder
	turnID  string
	pricing types.ProviderPricing

	mu       sync.Mutex
	llmCalls int
	entries  []toolEntry
}

func newToolAttributor(rec *trace.Recorder, turnID string) *toolAttributor {
	if rec == nil {
		// Absence rather than a flag: every method tolerates nil, so
		// the agent loop calls them unconditionally.
		return nil
	}
	return &toolAttributor{rec: rec, turnID: turnID}
}

// noteLLMCall records that a prompt went out.
//
// Counted here rather than derived from the span stream, because the
// span stream may have dropped spans under load and an attribution
// computed from a lossy count would be quietly wrong rather than
// visibly missing.
func (t *toolAttributor) noteLLMCall(pricing types.ProviderPricing) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.llmCalls++
	// The last provider to serve the turn prices the carry. A turn
	// that failed over mid-way was re-prompted at the new provider's
	// rate, and that is the rate the carried context actually cost.
	// Compared field-by-field rather than with ==: ProviderPricing
	// gained a map for non-token units, and a struct holding one is
	// not comparable.
	if pricing.IsSet() {
		t.pricing = pricing
	}
}

// noteTool records a tool result entering the message list, and emits
// the tool_call span immediately.
//
// Immediately, because a turn that times out or blows its budget is
// exactly the turn somebody will want the trace of, and buffering the
// span until the end would lose it on every path that matters. The
// ATTRIBUTION is what waits — it cannot be known until the turn ends —
// and it arrives as its own span.
func (t *toolAttributor) noteTool(inv ToolInvocation, elapsed time.Duration, started time.Time) {
	if t == nil {
		return
	}
	spanID := ids.New()
	resultBytes := len(inv.Output) + len(inv.Error)
	// Sized from the message the agent will actually append, so the
	// estimate matches what the provider is billed for rather than the
	// raw tool output.
	resultTokens := estimateTokens(Message{Role: "tool", Content: inv.Output})

	t.mu.Lock()
	t.entries = append(t.entries, toolEntry{
		spanID:       spanID,
		name:         inv.ToolName,
		resultBytes:  resultBytes,
		resultTokens: resultTokens,
		afterCall:    t.llmCalls,
	})
	t.mu.Unlock()

	outcome := trace.OutcomeOK
	if inv.Error != "" {
		// Aborted rather than advanced: a failing tool is not retried
		// against another provider, so nothing "advanced".
		outcome = trace.OutcomeAborted
	}
	t.rec.Record(trace.Span{
		TurnID:     t.turnID,
		SpanID:     spanID,
		Kind:       trace.KindToolCall,
		Name:       inv.ToolName,
		StartedAt:  started,
		Duration:   elapsed,
		Outcome:    outcome,
		ResultSize: resultBytes,
		Usage:      trace.Usage{PromptTokens: resultTokens},
		Error:      inv.Error,
	})
}

// flush emits one context-carry span per tool.
//
// Called from a defer on every exit path — normal, budget-exceeded,
// confirmation, timeout — because a turn that ended unusually is the
// one whose cost somebody is asking about.
//
// A tool carried in zero later prompts still gets a span, with zero
// cost. Omitting it would make "this tool was free" indistinguishable
// from "this tool was not recorded", and the first is a genuine and
// useful answer: a tool called on the final round-trip is paid for
// once, in the reply, and never re-sent.
func (t *toolAttributor) flush() {
	if t == nil {
		return
	}
	t.mu.Lock()
	entries := append([]toolEntry(nil), t.entries...)
	llmCalls := t.llmCalls
	pricing := t.pricing
	t.entries = nil
	t.mu.Unlock()

	for _, e := range entries {
		// Present in the prompt of every call after the one that
		// produced it. A result from call 2 of 6 rides in calls 3..6,
		// so four carries.
		carries := max(llmCalls-e.afterCall, 0)
		carried := e.resultTokens * carries
		t.rec.Record(trace.Span{
			TurnID:   t.turnID,
			SpanID:   ids.New(),
			ParentID: e.spanID,
			Kind:     trace.KindContextCarry,
			Name:     e.name,
			Outcome:  trace.OutcomeOK,
			Usage:    trace.Usage{PromptTokens: carried},
			// Priced at the PROMPT rate, because that is what re-sent
			// context is: input tokens on every subsequent call. Using
			// the completion rate here would overstate it several-fold.
			CostUSD:  EstimateCost(Usage{PromptTokens: carried}, pricing),
			Attempt:  carries,
			Unit:     "prompt_resends",
			Quantity: float64(carries),
		})
	}
}
