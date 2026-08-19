package compute

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// The thing Dream was waiting for.
//
// DreamRunner has scored, pruned and digested since it was written,
// and never once consolidated: the step is gated on a Summarizer,
// SetSummarizer was called from nothing outside tests, and no type in
// the tree implemented the interface. The comment said "Phase 3.3
// ships no real implementation — wiring waits for Phase 5". This is
// that implementation.
//
// NO EMBEDDER. Dream's candidate selection is recency times
// importance and touches no vector; the embedding is metadata on the
// OUTPUT record, so a summary can later be found by vector search.
// Returning none leaves the summary findable lexically, which is what
// memory_search already falls back to on a node with no embedder —
// and a consolidation nobody can vector-search is worth incomparably
// more than the nothing that exists today.

// dreamSummaryPrompt asks for the consolidation.
//
// "What happened" rather than "summarise these" because the input is
// a set of episodes from one person's life with an assistant, and a
// summary written as a list of messages is a transcript with fewer
// words. The value of a consolidation is that it says what the
// episodes AMOUNT to.
const dreamSummaryPrompt = `You are consolidating episodic memories for a personal assistant.

Below are separate things that happened, in no particular order. Write ONE short paragraph recording what they amount to — durable facts, preferences, decisions and recurring themes worth remembering later.

Rules:
- Write it as settled knowledge, not as a description of a conversation. "John prefers British spelling" — not "the user said they prefer British spelling".
- Keep what would still matter in a month. Drop pleasantries, one-off questions already answered, and anything already obvious.
- If these episodes amount to nothing worth keeping, reply with exactly: NOTHING
- No preamble, no headings, no bullet points. One paragraph.`

// dreamSummaryMaxTokens bounds the consolidation.
//
// A consolidation longer than the episodes it replaces is not a
// consolidation. Generous enough for a paragraph about a dozen
// episodes and no more.
const dreamSummaryMaxTokens = 400

// maxDreamEvents caps how many episodes reach the model.
//
// The runner already selects a top-N by score, but that limit is
// configurable and this one is about the request: an unbounded set of
// episodes is an unbounded prompt, and the cost of a nightly pass
// should not scale with how much happened that day.
const maxDreamEvents = 40

// DreamSummarizer consolidates episodes through an LLM.
type DreamSummarizer struct {
	provider LLMProvider
	model    string
	log      *slog.Logger
}

// NewDreamSummarizer builds one. A nil provider returns nil, which
// Dream reads as "no summarizer" and goes on scoring and pruning
// without consolidating — the behaviour it has had all along.
func NewDreamSummarizer(p LLMProvider, model string, log *slog.Logger) *DreamSummarizer {
	if p == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &DreamSummarizer{provider: p, model: model, log: log}
}

// Summarize implements memory.Summarizer.
//
// The empty summary is a documented answer, not a failure: Dream
// skips the write when it gets one, so a pass over episodes that
// amount to nothing records nothing rather than a paragraph saying
// so.
func (s *DreamSummarizer) Summarize(ctx context.Context, events []string) (string, []float32, error) {
	if s == nil || s.provider == nil {
		return "", nil, nil
	}
	kept := make([]string, 0, len(events))
	for _, e := range events {
		if strings.TrimSpace(e) == "" {
			continue
		}
		kept = append(kept, e)
		if len(kept) == maxDreamEvents {
			break
		}
	}
	if len(kept) == 0 {
		return "", nil, nil
	}

	var b strings.Builder
	for i, e := range kept {
		fmt.Fprintf(&b, "%d. %s\n", i+1, strings.TrimSpace(e))
	}

	resp, err := s.provider.Chat(ctx, ChatRequest{
		Model:     s.model,
		MaxTokens: dreamSummaryMaxTokens,
		Messages: []Message{
			{Role: "system", Content: dreamSummaryPrompt},
			{Role: "user", Content: b.String()},
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("dream summary: %w", err)
	}

	summary := strings.TrimSpace(resp.Content)
	if isNothingWorthKeeping(summary) {
		s.log.Debug("dream: the episodes amounted to nothing worth keeping", "events", len(kept))
		return "", nil, nil
	}
	// No embedding: see the note at the head of this file.
	return summary, nil, nil
}

// isNothingWorthKeeping reads the refusal.
//
// Matched on the whole trimmed reply rather than by substring: a
// consolidation that legitimately contains the word "nothing" —
// "nothing came of the London trip" — is a summary, not a refusal.
func isNothingWorthKeeping(summary string) bool {
	if summary == "" {
		return true
	}
	// Emphasis and punctuation a model adds unasked. Getting this set
	// wrong costs a memory: an unrecognised refusal is written to the
	// store as though it were a consolidation.
	trimmed := strings.Trim(strings.ToUpper(summary), ".!*_#\"'` \t\n")
	return trimmed == "NOTHING"
}
