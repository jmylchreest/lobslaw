package compute

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"
)

// The routing signal.
//
// [[compute.chains]] triggers on min_complexity and domains, and
// nothing in the system produced either — so of the three trigger
// kinds only `always` could ever fire, and chains could not route.
// This is what produces them.
//
// A cheap model rather than a heuristic, because the thing being
// judged is how hard a question is, and message length is a poor
// proxy for that: "prove this terminates" is short and hard, a pasted
// stack trace is long and easy. The preflight role already existed for
// exactly this shape of work — a small, fast model doing classification
// ahead of the main turn — and it falls back to main when unset.

// Hint is the coarse routing vocabulary, mapped onto chains an
// operator can inspect and override. Sugar over chains, not a second
// mental model: a hint selects a chain, it does not select a model.
type Hint string

const (
	HintFast      Hint = "fast"
	HintBalanced  Hint = "balanced"
	HintDeep      Hint = "deep"
	HintReasoning Hint = "reasoning"
)

// Valid reports whether h is one of the four. Anything else from a
// model is discarded rather than passed on — an unrecognised hint that
// reached the resolver would match no chain and silently route to the
// default, which looks exactly like the preflight having no opinion.
func (h Hint) Valid() bool {
	switch h {
	case HintFast, HintBalanced, HintDeep, HintReasoning:
		return true
	}
	return false
}

// Complexity thresholds for deriving a hint when the model offered
// none. HintReasoning is deliberately absent: it is a request for a
// different KIND of model, not a point on a difficulty scale, so it is
// only ever honoured when something asked for it by name.
const (
	complexityBalanced = 30
	complexityDeep     = 70
)

// Judgment is what routing decides on.
type Judgment struct {
	// Complexity is 0-100. Chains with a MinComplexity trigger match
	// when Complexity >= trigger.
	Complexity int
	// Domains are free-form tags — "code", "finance", "legal".
	Domains []string
	// Hint selects a chain directly when set.
	Hint Hint
}

// NeutralJudgment is "no opinion": no chain triggers on it, so the
// default route applies.
//
// This is what a failed or nonsensical preflight returns. A turn must
// not die because the thing that decides how to route it was
// unavailable — the answer to "I don't know how hard this is" is to
// answer it the normal way.
func NeutralJudgment() Judgment {
	return Judgment{Hint: HintBalanced}
}

// judgeSystemPrompt asks for a compact object. Models are told the
// scale in concrete terms because "rate complexity 0-100" without
// anchors produces a cluster around 50 and no discrimination.
const judgeSystemPrompt = `You classify requests for routing. Reply with JSON only, no prose, no code fences:

{"complexity": <0-100>, "domains": [<tags>], "hint": "fast"|"balanced"|"deep"|"reasoning"}

complexity: 0-20 greeting, acknowledgement, or a fact you could answer in one line. 21-50 a normal question needing a paragraph. 51-80 multi-step work, unfamiliar material, or careful reasoning. 81-100 research, proof, architecture, or anything where being wrong is expensive.

Judge the DIFFICULTY, not the length. "Prove this loop terminates" is short and hard. A pasted stack trace is long and easy.

domains: at most 3 lowercase single-word tags naming the subject area ("code", "finance", "legal", "medical", "maths"). Omit rather than guess.

hint: "reasoning" only when the task needs sustained deduction rather than knowledge. Otherwise pick from the complexity.`

// judgeMaxCompletionTokens bounds the reply. The answer is one small
// object; a model that pads still yields a parseable prefix.
const judgeMaxCompletionTokens = 128

// judgeTimeout bounds the preflight independently of the turn.
//
// The preflight exists to make the turn better, so it must not be able
// to make it slower than the turn would have been alone. A judge that
// hangs yields the neutral judgment and the turn proceeds.
const judgeTimeout = 8 * time.Second

// maxDomains bounds what a model can put in the routing key.
const maxDomains = 3

// Judge classifies a turn for routing. A nil *Judge is usable and
// returns the neutral judgment, so call sites do not branch on whether
// a preflight provider was configured.
type Judge struct {
	provider LLMProvider
	model    string
	// timeout overrides judgeTimeout when the operator set one for
	// this role. Zero keeps the constant.
	timeout time.Duration
	log     *slog.Logger
}

// NewJudge wires a judge to the preflight provider. A nil provider
// gives a nil judge — absence, not a disabled flag.
func NewJudge(provider LLMProvider, model string, timeout time.Duration, log *slog.Logger) *Judge {
	if provider == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Judge{provider: provider, model: model, timeout: timeout, log: log}
}

// Judge classifies text, honouring an explicit hint.
//
// An explicit hint SKIPS THE CALL. Somebody who said "deep" has
// already answered the only question the preflight was going to ask,
// and paying for a model to second-guess them would be both slower and
// worse — the hint is the strong prior, not a suggestion to weigh.
func (j *Judge) Judge(ctx context.Context, text string, explicit Hint) Judgment {
	if explicit.Valid() {
		return Judgment{Complexity: complexityOf(explicit), Hint: explicit}
	}
	if j == nil || strings.TrimSpace(text) == "" {
		return NeutralJudgment()
	}

	ctx, cancel := context.WithTimeout(ctx, orDefault(j.timeout, judgeTimeout))
	defer cancel()

	resp, err := j.provider.Chat(ctx, ChatRequest{
		Model:       j.model,
		MaxTokens:   judgeMaxCompletionTokens,
		Temperature: 0,
		Messages: []Message{
			{Role: "system", Content: judgeSystemPrompt},
			{Role: "user", Content: text},
		},
	})
	if err != nil || resp == nil {
		// DEBUG, not WARN. A preflight that fails costs routing
		// precision, not correctness, and a provider having a bad
		// minute should not fill an operator's log with warnings about
		// turns that all completed.
		j.log.Debug("preflight: judge unavailable; routing on the default", "error", err)
		return NeutralJudgment()
	}
	return parseJudgment(resp.Content, j.log)
}

// complexityOf gives an explicit hint a score, so a hint and a
// preflight judgment are the same shape downstream and a chain with a
// MinComplexity trigger still matches one.
func complexityOf(h Hint) int {
	switch h {
	case HintFast:
		return 10
	case HintDeep, HintReasoning:
		return 85
	default:
		return 50
	}
}

// parseJudgment reads the model's object, discarding anything it got
// wrong rather than propagating it.
func parseJudgment(content string, log *slog.Logger) Judgment {
	raw := extractObject(content)
	if raw == "" {
		log.Debug("preflight: no JSON object in the reply; routing on the default")
		return NeutralJudgment()
	}
	var parsed struct {
		Complexity int      `json:"complexity"`
		Domains    []string `json:"domains"`
		Hint       string   `json:"hint"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Debug("preflight: unparseable judgment; routing on the default", "error", err)
		return NeutralJudgment()
	}

	out := Judgment{Complexity: clampComplexity(parsed.Complexity)}
	if h := Hint(strings.ToLower(strings.TrimSpace(parsed.Hint))); h.Valid() {
		out.Hint = h
	} else {
		out.Hint = hintFor(out.Complexity)
	}
	out.Domains = normaliseDomains(parsed.Domains)
	return out
}

// extractObject finds the first balanced {...} run.
//
// Models wrap JSON in code fences and prose however firmly they are
// told not to, and a reply that is right apart from three backticks is
// a reply worth reading.
func extractObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside a string are not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func clampComplexity(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func hintFor(complexity int) Hint {
	switch {
	case complexity < complexityBalanced:
		return HintFast
	case complexity < complexityDeep:
		return HintBalanced
	default:
		return HintDeep
	}
}

// normaliseDomains lowercases, trims, drops blanks and duplicates, and
// bounds the count.
//
// These become a routing key. A model that returns "Code" one turn and
// "code" the next would route the same question two different ways,
// and an operator comparing the two would have nothing to look at.
func normaliseDomains(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, maxDomains)
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
		if len(out) == maxDomains {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
