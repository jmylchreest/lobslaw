package soul

import (
	"context"
	"errors"
	"testing"
)

func TestRegexClassifierSarcasmDecrease(t *testing.T) {
	t.Parallel()
	c := NewRegexClassifier()
	fb, err := c.Classify(context.Background(), "please be less snarky")
	if err != nil {
		t.Fatal(err)
	}
	if fb.Dimension != "sarcasm" {
		t.Errorf("dimension: %q", fb.Dimension)
	}
	if fb.Direction != DirectionDecrease {
		t.Errorf("direction: %d", fb.Direction)
	}
}

func TestRegexClassifierFormalityIncrease(t *testing.T) {
	t.Parallel()
	c := NewRegexClassifier()
	fb, err := c.Classify(context.Background(), "can you be more formal with me")
	if err != nil {
		t.Fatal(err)
	}
	if fb.Dimension != "formality" || fb.Direction != DirectionIncrease {
		t.Errorf("got %+v", fb)
	}
}

func TestRegexClassifierHumorDecrease(t *testing.T) {
	t.Parallel()
	c := NewRegexClassifier()
	fb, err := c.Classify(context.Background(), "less jokes please")
	if err != nil {
		t.Fatal(err)
	}
	if fb.Dimension != "humor" || fb.Direction != DirectionDecrease {
		t.Errorf("got %+v", fb)
	}
}

func TestRegexClassifierUnclassifiable(t *testing.T) {
	t.Parallel()
	c := NewRegexClassifier()
	_, err := c.Classify(context.Background(), "what's the weather today")
	if !errors.Is(err, ErrNoClassification) {
		t.Errorf("want ErrNoClassification; got %v", err)
	}
}

func TestRegexClassifierEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewRegexClassifier()
	_, err := c.Classify(context.Background(), "   ")
	if !errors.Is(err, ErrNoClassification) {
		t.Errorf("want ErrNoClassification for empty; got %v", err)
	}
}

// --- LLM classifier ------------------------------------------------------

func TestLLMClassifierCallsCallback(t *testing.T) {
	t.Parallel()
	var seen string
	callback := func(_ context.Context, prompt string) (string, error) {
		seen = prompt
		return "sarcasm decrease", nil
	}
	c := NewLLMClassifier(callback)
	fb, err := c.Classify(context.Background(), "stop being cheeky")
	if err != nil {
		t.Fatal(err)
	}
	if fb.Dimension != "sarcasm" || fb.Direction != DirectionDecrease {
		t.Errorf("got %+v", fb)
	}
	if seen == "" {
		t.Error("callback didn't see a prompt")
	}
}

func TestLLMClassifierFallsBackOnError(t *testing.T) {
	t.Parallel()
	callback := func(_ context.Context, _ string) (string, error) {
		return "", errors.New("LLM unavailable")
	}
	c := NewLLMClassifier(callback)
	// A regex-classifiable phrase should succeed via fallback.
	fb, err := c.Classify(context.Background(), "please be less snarky")
	if err != nil {
		t.Fatalf("fallback should succeed; got %v", err)
	}
	if fb.Dimension != "sarcasm" {
		t.Errorf("fallback should hit regex; got %+v", fb)
	}
}

func TestLLMClassifierFallsBackOnUnparseable(t *testing.T) {
	t.Parallel()
	callback := func(_ context.Context, _ string) (string, error) {
		return "I'm not sure what you want, could you clarify?", nil
	}
	c := NewLLMClassifier(callback)
	fb, err := c.Classify(context.Background(), "please be less snarky")
	if err != nil {
		t.Fatalf("fallback should have classified: %v", err)
	}
	if fb.Dimension != "sarcasm" {
		t.Errorf("fallback should land on sarcasm; got %+v", fb)
	}
}

func TestLLMClassifierNoneResponse(t *testing.T) {
	t.Parallel()
	callback := func(_ context.Context, _ string) (string, error) {
		return "none", nil
	}
	c := NewLLMClassifier(callback)
	// LLM says "none" but regex ALSO can't classify → ErrNoClassification.
	_, err := c.Classify(context.Background(), "what is the weather")
	if !errors.Is(err, ErrNoClassification) {
		t.Errorf("want ErrNoClassification; got %v", err)
	}
}

// TestLLMClassifierNilCallbackGoesStraightToFallback — configured
// for llm mode but no callback wired is a valid operator state
// (fast-tier provider not configured yet). Behaves as pure regex.
func TestLLMClassifierNilCallbackGoesStraightToFallback(t *testing.T) {
	t.Parallel()
	c := NewLLMClassifier(nil)
	fb, err := c.Classify(context.Background(), "please be less snarky")
	if err != nil {
		t.Fatal(err)
	}
	if fb.Dimension != "sarcasm" {
		t.Errorf("expected sarcasm fallback; got %+v", fb)
	}
}

func TestParseLLMResponseEdgeCases(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"sarcasm decrease":         true,
		"SARCASM INCREASE":         true,
		" sarcasm decrease.":       true,  // trailing punctuation stripped
		"sarcasm down":             true,  // synonym direction
		"formality up":             true,
		"none":                     false,
		"":                         false,
		"sarcasm":                  false, // missing direction
		"unknown_dim decrease":     false,
		"sarcasm sideways":         false,
		"too many tokens here":     false,
	}
	for input, want := range cases {
		fb := parseLLMResponse(input, "orig")
		got := fb != nil
		if got != want {
			t.Errorf("%q: got %v want %v", input, got, want)
		}
	}
}
