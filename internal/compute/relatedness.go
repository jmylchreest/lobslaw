package compute

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// Is this the rest of the question, or a different one?
//
// The cheapest classification in the system, and deliberately kept
// that way: it runs per MESSAGE rather than per turn, including on
// messages that arrive while nothing is running. Anything expensive
// here is paid on every keystroke-burst a user makes.

// relatednessSystemPrompt asks for one word.
//
// One word rather than JSON because the answer is one bit and a
// schema would cost tokens to express what "yes" already says. An
// unparseable reply is treated as "no opinion" by the caller, which
// folds — so the failure mode of a chatty model is debounce, not a
// lost message.
const relatednessSystemPrompt = `You decide whether a new chat message continues the previous message(s) or starts a separate request.

Answer with exactly one word: SAME or NEW.

SAME — the new message adds to, clarifies, or asks more about the same subject.
  "what's the weather in York?" then "what about rain? wind?" -> SAME
  "what's the weather?" then "sunset? sunrise?" -> SAME

NEW — the new message is a different request that deserves its own answer.
  "what's the weather in York?" then "also cancel the 3pm meeting" -> NEW
  "summarise this document" then "what time is it in Tokyo?" -> NEW

When genuinely unsure, answer SAME.`

// relatednessMaxTokens bounds the reply. One word needs very few, and
// a cap this low also stops a model that ignores the instruction from
// narrating at the user's expense.
const relatednessMaxTokens = 4

// RelatednessJudge classifies an arriving message against the ones
// already collected.
type RelatednessJudge struct {
	provider LLMProvider
	model    string
	log      *slog.Logger
}

// NewRelatednessJudge builds one. A nil provider returns nil, which
// the gate reads as "no judge" and falls back to folding.
func NewRelatednessJudge(p LLMProvider, model string, log *slog.Logger) *RelatednessJudge {
	if p == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &RelatednessJudge{provider: p, model: model, log: log}
}

// Related reports whether incoming belongs with pending.
//
// Errors are returned rather than swallowed so the caller can log the
// reason once and apply its own fallback — which is to fold, because
// debounce is the behaviour this refines.
func (j *RelatednessJudge) Related(ctx context.Context, pending []string, incoming string) (bool, error) {
	if j == nil || j.provider == nil {
		return true, nil
	}
	prior := strings.TrimSpace(strings.Join(pending, "\n"))
	if prior == "" {
		return true, nil
	}
	resp, err := j.provider.Chat(ctx, ChatRequest{
		Model:     j.model,
		MaxTokens: relatednessMaxTokens,
		Messages: []Message{
			{Role: "system", Content: relatednessSystemPrompt},
			// Labelled rather than concatenated: without the labels
			// the model sees one run-on message and has nothing to
			// compare against.
			{Role: "user", Content: fmt.Sprintf("PREVIOUS:\n%s\n\nNEW:\n%s", prior, strings.TrimSpace(incoming))},
		},
	})
	if err != nil {
		return true, err
	}
	return parseRelatedness(resp.Content), nil
}

// parseRelatedness reads the one-word answer.
//
// Anything that is not recognisably NEW counts as SAME. The bias is
// deliberate and matches the prompt's own instruction: folding two
// related messages costs nothing, and splitting one thought answers
// half of it.
func parseRelatedness(content string) bool {
	switch {
	case strings.Contains(strings.ToUpper(content), "NEW"):
		return false
	default:
		return true
	}
}
