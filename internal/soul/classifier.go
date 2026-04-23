package soul

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
)

// Direction is the sign of a dimension adjustment. Increase means
// "more of this trait"; Decrease means "less."
type Direction int

const (
	DirectionIncrease Direction = 1
	DirectionDecrease Direction = -1
)

// Feedback is one classified adjustment request. Dimension is the
// field on types.EmotiveStyle being tuned — one of excitement,
// formality, directness, sarcasm, humor. Reason carries the
// originating phrase so audit + confirmations can surface what
// the user actually said.
type Feedback struct {
	Dimension string
	Direction Direction
	Reason    string
}

// Classifier maps free-text user feedback ("stop being so snarky")
// into a structured Feedback. An implementation that can't make
// sense of the input returns (nil, ErrNoClassification) so the
// agent doesn't apply a mystery adjustment.
type Classifier interface {
	Classify(ctx context.Context, utterance string) (*Feedback, error)
}

// ErrNoClassification signals "I can't tell what dimension the
// user wants adjusted." Callers surface this to the user rather
// than guessing.
var ErrNoClassification = errors.New("soul: no classification match")

// Dimensions is the canonical ordering of classifiable dimensions.
// Used as the menu for the LLM classifier's prompt + as the set
// the regex classifier iterates over.
var Dimensions = []string{
	"excitement",
	"formality",
	"directness",
	"sarcasm",
	"humor",
}

// RegexClassifier is the offline / fallback classifier. Matches
// phrasing against a compiled set of patterns per dimension. Not as
// good as the LLM path on novel phrasings, but cheap, deterministic,
// and always available — which is why it's the fallback the LLM
// path delegates to on any classifier-call error.
type RegexClassifier struct {
	patterns []dimensionPattern
}

// dimensionPattern pairs an emotive dimension with the regex set
// that detects adjustment intent for it. polarity flips direction
// when the operator expresses "less" vs "more".
type dimensionPattern struct {
	dimension string
	// decrease matches "be less X", "stop being so X", "don't X"
	decrease *regexp.Regexp
	// increase matches "be more X", "really X", "can you X more"
	increase *regexp.Regexp
}

// NewRegexClassifier builds the default English-language classifier.
// Patterns live here rather than in config because the vocabulary
// is tightly coupled to the dimension semantics; an operator who
// wanted a truly different pattern set would fork or swap in a
// custom Classifier via SetClassifier on the adjustment engine.
func NewRegexClassifier() *RegexClassifier {
	return &RegexClassifier{
		patterns: []dimensionPattern{
			{
				dimension: "sarcasm",
				decrease:  regexp.MustCompile(`(?i)\b(?:less|not so|stop being so|don'?t be so)\s+(?:snarky|sarcastic|snide)\b`),
				increase:  regexp.MustCompile(`(?i)\b(?:more|be more|extra)\s+(?:snarky|sarcastic|snide|cheeky)\b`),
			},
			{
				dimension: "formality",
				decrease:  regexp.MustCompile(`(?i)\b(?:less formal|not so formal|be more casual|chill out|relax)\b`),
				increase:  regexp.MustCompile(`(?i)\b(?:more formal|be formal|professional tone|polite)\b`),
			},
			{
				dimension: "directness",
				decrease:  regexp.MustCompile(`(?i)\b(?:less blunt|soften|less direct|gentler)\b`),
				increase:  regexp.MustCompile(`(?i)\b(?:more direct|be blunt|cut to the chase|straight up)\b`),
			},
			{
				dimension: "humor",
				decrease:  regexp.MustCompile(`(?i)\b(?:less (?:jokes|funny)|not funny|stop joking|serious)\b`),
				increase:  regexp.MustCompile(`(?i)\b(?:more jokes|funnier|be funny|lighten up)\b`),
			},
			{
				dimension: "excitement",
				decrease:  regexp.MustCompile(`(?i)\b(?:less excited|calm down|dial it back|chill)\b`),
				increase:  regexp.MustCompile(`(?i)\b(?:more excited|more energy|pump it up|enthusiastic)\b`),
			},
		},
	}
}

// Classify tries every registered pattern in order. First match wins
// — ordering is deliberate: sarcasm before formality because
// "less snarky" would otherwise trigger formality's
// "less (anything)" variants if the latter weren't more specific.
func (c *RegexClassifier) Classify(_ context.Context, utterance string) (*Feedback, error) {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return nil, ErrNoClassification
	}
	for _, p := range c.patterns {
		if p.decrease.MatchString(utterance) {
			return &Feedback{
				Dimension: p.dimension,
				Direction: DirectionDecrease,
				Reason:    utterance,
			}, nil
		}
		if p.increase.MatchString(utterance) {
			return &Feedback{
				Dimension: p.dimension,
				Direction: DirectionIncrease,
				Reason:    utterance,
			}, nil
		}
	}
	return nil, ErrNoClassification
}

// LLMClassifyCallback is the narrow hook the LLM classifier uses to
// reach the fast-tier provider. Separate from the agent loop's
// Provider interface because we don't need chat history / tool
// calls / budget accounting — just a short one-shot classification
// request. Injecting rather than importing compute keeps soul free
// of a dependency cycle.
type LLMClassifyCallback func(ctx context.Context, prompt string) (string, error)

// LLMClassifier calls the LLM to map an utterance to (dimension,
// direction). Falls back to the regex classifier if the LLM call
// returns an error or an unparseable response — "the LLM is down"
// and "the LLM said something weird" both reduce to "use the
// offline path," keeping the caller unaware of the internal split.
type LLMClassifier struct {
	callback LLMClassifyCallback
	fallback Classifier
}

// NewLLMClassifier builds a classifier that prefers the LLM and
// falls back to NewRegexClassifier on any error. callback may be
// nil — in that case every call goes straight to the fallback,
// which is the configured behaviour for "no LLM provider wired
// yet but operator picked classifier=llm anyway."
func NewLLMClassifier(callback LLMClassifyCallback) *LLMClassifier {
	return &LLMClassifier{callback: callback, fallback: NewRegexClassifier()}
}

// Classify asks the LLM to emit one line: "dimension direction"
// (e.g. "sarcasm decrease"). Anything else — multi-line output,
// extra tokens, a refusal — drops through to the regex fallback.
// The prompt is deliberately terse; fast-tier models handle this
// shape well and the terseness keeps token cost minimal.
func (c *LLMClassifier) Classify(ctx context.Context, utterance string) (*Feedback, error) {
	if c.callback == nil {
		return c.fallback.Classify(ctx, utterance)
	}
	prompt := buildClassifyPrompt(utterance)
	raw, err := c.callback(ctx, prompt)
	if err != nil {
		return c.fallback.Classify(ctx, utterance)
	}
	if fb := parseLLMResponse(raw, utterance); fb != nil {
		return fb, nil
	}
	return c.fallback.Classify(ctx, utterance)
}

// buildClassifyPrompt formats the menu + request. Kept as a
// package-private helper so tests can assert on its contents
// independently of the classifier's runtime behaviour.
func buildClassifyPrompt(utterance string) string {
	return strings.Join([]string{
		"Classify the user's feedback into one of the following emotive dimensions: " +
			strings.Join(Dimensions, ", ") + ".",
		"Reply with exactly two tokens separated by a space: the dimension name, then 'increase' or 'decrease'.",
		"If you cannot classify, reply with exactly 'none'.",
		"User feedback: " + utterance,
	}, "\n")
}

// parseLLMResponse decodes the expected "dimension direction" form.
// Tolerant of extra whitespace / surrounding punctuation the model
// might emit despite the prompt. Returns nil on any unrecognised
// shape so the caller falls back.
func parseLLMResponse(raw, utterance string) *Feedback {
	trimmed := strings.TrimSpace(strings.ToLower(raw))
	trimmed = strings.Trim(trimmed, ".:;\"'")
	if trimmed == "none" || trimmed == "" {
		return nil
	}
	fields := strings.Fields(trimmed)
	if len(fields) != 2 {
		return nil
	}
	dim := fields[0]
	if !isKnownDimension(dim) {
		return nil
	}
	var dir Direction
	switch fields[1] {
	case "increase", "up", "more":
		dir = DirectionIncrease
	case "decrease", "down", "less":
		dir = DirectionDecrease
	default:
		return nil
	}
	return &Feedback{Dimension: dim, Direction: dir, Reason: utterance}
}

func isKnownDimension(name string) bool {
	return slices.Contains(Dimensions, name)
}
