package compute

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// The preflight is what makes chains route at all: nothing else in the
// system produces a complexity score or a domain tag, so before this
// only an `always` trigger could ever fire.
//
// Its failures must be cheap. A turn dying because the thing that
// decides HOW to route it was unavailable would be worse than routing
// every turn down the default.

// Judgment holds a slice, so it is not comparable with ==.
func isNeutral(j Judgment) bool {
	n := NeutralJudgment()
	return j.Complexity == n.Complexity && j.Hint == n.Hint && len(j.Domains) == 0
}

type scriptedLLM struct {
	reply string
	err   error
	calls int
	seen  ChatRequest
}

func (s *scriptedLLM) Chat(_ context.Context, req ChatRequest) (*ChatResponse, error) {
	s.calls++
	s.seen = req
	if s.err != nil {
		return nil, s.err
	}
	return &ChatResponse{Content: s.reply}, nil
}

func judgeWith(t *testing.T, reply string) (*Judge, *scriptedLLM) {
	t.Helper()
	llm := &scriptedLLM{reply: reply}
	return NewJudge(llm, "tiny", 0, slog.Default()), llm
}

func TestAJudgmentRoutes(t *testing.T) {
	t.Parallel()
	j, _ := judgeWith(t, `{"complexity": 82, "domains": ["code","maths"], "hint": "deep"}`)
	got := j.Judge(context.Background(), "prove this loop terminates", "")
	if got.Complexity != 82 || got.Hint != HintDeep {
		t.Errorf("got %+v", got)
	}
	if len(got.Domains) != 2 || got.Domains[0] != "code" {
		t.Errorf("domains = %v", got.Domains)
	}
}

// Somebody who said "deep" has already answered the only question the
// preflight was going to ask. Paying a model to second-guess them is
// slower and worse.
func TestAnExplicitHintSkipsTheCallEntirely(t *testing.T) {
	t.Parallel()
	j, llm := judgeWith(t, `{"complexity": 5, "hint": "fast"}`)
	got := j.Judge(context.Background(), "anything", HintDeep)
	if llm.calls != 0 {
		t.Errorf("called the provider %d times; an explicit hint should skip it", llm.calls)
	}
	if got.Hint != HintDeep {
		t.Errorf("hint = %q; the explicit hint was overruled", got.Hint)
	}
	// It still needs a score, or a chain triggering on MinComplexity
	// would never match a hinted turn.
	if got.Complexity < complexityDeep {
		t.Errorf("complexity = %d; a deep hint should satisfy a deep chain's trigger", got.Complexity)
	}
}

// A hint nobody defined is not a hint. Passing it on would match no
// chain and route to the default, which looks identical to the
// preflight having had no opinion.
func TestAnUnknownExplicitHintDoesNotSkipTheCall(t *testing.T) {
	t.Parallel()
	j, llm := judgeWith(t, `{"complexity": 40}`)
	got := j.Judge(context.Background(), "a question", Hint("turbo"))
	if llm.calls != 1 {
		t.Errorf("calls = %d; an unrecognised hint should fall through to the judge", llm.calls)
	}
	if got.Hint == Hint("turbo") {
		t.Error("an unrecognised hint reached the resolver")
	}
}

// --- failure is not fatal ------------------------------------------

func TestAFailedJudgeRoutesOnTheDefault(t *testing.T) {
	t.Parallel()
	llm := &scriptedLLM{err: errors.New("429 slow down")}
	j := NewJudge(llm, "tiny", 0, slog.Default())
	got := j.Judge(context.Background(), "a question", "")
	if !isNeutral(got) {
		t.Errorf("got %+v, want the neutral judgment", got)
	}
}

func TestGarbageRoutesOnTheDefault(t *testing.T) {
	t.Parallel()
	for name, reply := range map[string]string{
		"prose":         "I think this is a fairly hard question, actually.",
		"broken json":   `{"complexity": 8`,
		"empty":         "",
		"wrong types":   `{"complexity": "very high", "domains": "code"}`,
		"unclosed pair": `{"complexity": 50, "domains": [`,
	} {
		got := parseJudgment(reply, slog.Default())
		if got.Hint != HintBalanced || got.Complexity != 0 || got.Domains != nil {
			t.Errorf("%s: got %+v, want the neutral judgment", name, got)
		}
	}
}

// A nil judge is usable, so no call site has to branch on whether a
// preflight provider was configured.
func TestANilJudgeIsUsable(t *testing.T) {
	t.Parallel()
	if NewJudge(nil, "", 0, nil) != nil {
		t.Fatal("a nil provider should give a nil judge")
	}
	var j *Judge
	if got := j.Judge(context.Background(), "a question", ""); !isNeutral(got) {
		t.Errorf("got %+v, want the neutral judgment", got)
	}
	// An explicit hint still works without a provider — it needed no
	// model in the first place.
	if got := j.Judge(context.Background(), "a question", HintFast); got.Hint != HintFast {
		t.Errorf("hint = %q; an explicit hint needs no provider", got.Hint)
	}
}

// --- the model gets things wrong -----------------------------------

func TestFencedJSONIsStillRead(t *testing.T) {
	t.Parallel()
	got := parseJudgment("```json\n{\"complexity\": 75, \"hint\": \"deep\"}\n```", slog.Default())
	if got.Complexity != 75 || got.Hint != HintDeep {
		t.Errorf("got %+v; a reply that is right apart from backticks is worth reading", got)
	}
}

// A brace inside a string is not structure. Scanning for the last "}"
// or counting naively would truncate this one.
func TestBracesInsideStringsDoNotEndTheObject(t *testing.T) {
	t.Parallel()
	got := parseJudgment(`{"domains": ["a}b"], "complexity": 60}`, slog.Default())
	if got.Complexity != 60 {
		t.Errorf("complexity = %d; the object was cut short at a brace in a string", got.Complexity)
	}
}

func TestComplexityIsClamped(t *testing.T) {
	t.Parallel()
	if got := parseJudgment(`{"complexity": 900}`, slog.Default()); got.Complexity != 100 {
		t.Errorf("complexity = %d, want 100", got.Complexity)
	}
	if got := parseJudgment(`{"complexity": -5}`, slog.Default()); got.Complexity != 0 {
		t.Errorf("complexity = %d, want 0", got.Complexity)
	}
}

// Domains become a routing key. "Code" one turn and "code" the next
// would route the same question two different ways.
func TestDomainsAreNormalised(t *testing.T) {
	t.Parallel()
	got := parseJudgment(`{"domains": ["  Code ", "CODE", "", "Finance", "legal", "maths"]}`,
		slog.Default())
	if len(got.Domains) != maxDomains {
		t.Fatalf("domains = %v, want %d after dedup and bounding", got.Domains, maxDomains)
	}
	for _, d := range got.Domains {
		if d != strings.ToLower(strings.TrimSpace(d)) {
			t.Errorf("domain %q was not normalised", d)
		}
	}
	if got.Domains[0] != "code" || got.Domains[1] != "finance" {
		t.Errorf("domains = %v; order and dedup are wrong", got.Domains)
	}
}

// A model that scores the turn but names no hint still has to route
// somewhere, and the score is the thing it was most sure about.
func TestAMissingHintIsDerivedFromTheComplexity(t *testing.T) {
	t.Parallel()
	for complexity, want := range map[int]Hint{
		0: HintFast, 29: HintFast,
		30: HintBalanced, 69: HintBalanced,
		70: HintDeep, 100: HintDeep,
	} {
		if got := hintFor(complexity); got != want {
			t.Errorf("hintFor(%d) = %q, want %q", complexity, got, want)
		}
	}
}

// "reasoning" asks for a different KIND of model, not a harder one, so
// no score should ever produce it on its own.
func TestReasoningIsNeverInferred(t *testing.T) {
	t.Parallel()
	for c := 0; c <= 100; c++ {
		if hintFor(c) == HintReasoning {
			t.Fatalf("complexity %d inferred the reasoning hint", c)
		}
	}
	// It is honoured when asked for by name.
	if got := parseJudgment(`{"complexity": 20, "hint": "reasoning"}`, slog.Default()); got.Hint != HintReasoning {
		t.Errorf("hint = %q; an explicit reasoning hint was dropped", got.Hint)
	}
}

// The preflight exists to make the turn better. It must not be able to
// make it slower than the turn would have been without it.
func TestTheJudgeIsBounded(t *testing.T) {
	t.Parallel()
	if judgeTimeout <= 0 {
		t.Fatal("the judge has no timeout; a hanging preflight would hang the turn")
	}
	llm := &scriptedLLM{reply: `{"complexity": 10}`}
	j := NewJudge(llm, "tiny", 0, slog.Default())
	j.Judge(context.Background(), "hello", "")
	if _, hasDeadline := context.Background().Deadline(); hasDeadline {
		t.Fatal("test assumption broken")
	}
	if llm.seen.MaxTokens != judgeMaxCompletionTokens {
		t.Errorf("MaxTokens = %d; an unbounded reply is an unbounded wait", llm.seen.MaxTokens)
	}
	if llm.seen.Temperature != 0 {
		t.Errorf("temperature = %v; routing should not vary run to run", llm.seen.Temperature)
	}
}

// The model invents a hint too. A vocabulary that only the CALLER is
// held to is not a vocabulary — an unrecognised hint from either side
// matches no chain and routes to the default, which is
// indistinguishable from the preflight having had no opinion.
func TestAnUnknownHintFromTheModelIsDiscarded(t *testing.T) {
	t.Parallel()
	got := parseJudgment(`{"complexity": 80, "hint": "turbo"}`, slog.Default())
	if !got.Hint.Valid() {
		t.Errorf("hint = %q; an invalid hint reached the resolver", got.Hint)
	}
	// Discarded means derived from the score, not blanked — the model
	// was confident about the difficulty even if it invented a word.
	if got.Hint != HintDeep {
		t.Errorf("hint = %q, want it derived from complexity 80", got.Hint)
	}
}

// An omitted hint is the same case and must behave the same way.
func TestAnOmittedHintIsDerivedNotBlanked(t *testing.T) {
	t.Parallel()
	got := parseJudgment(`{"complexity": 10}`, slog.Default())
	if got.Hint != HintFast {
		t.Errorf("hint = %q, want fast for complexity 10", got.Hint)
	}
}
