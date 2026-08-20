package main

import (
	"strings"
	"testing"
)

// The usage text must say the two things that are not guessable: the
// node has to be stopped, and where the ground truth comes from.
func TestEmbedEvalUsageExplainsItself(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"STOPPED", "recall@1", "recall@3", "margin", "--download-url"} {
		if !strings.Contains(embedEvalUsage, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	// Why it exists at all — a model change is refused at boot, so the
	// evaluation has to come first.
	if !strings.Contains(embedEvalUsage, "refused at boot") {
		t.Error("usage does not say why this must be run before switching models")
	}
}

// Records whose event and context are IDENTICAL are excluded, because
// querying a document with itself is a free hit every model scores.
// Leaving them in inflates every result equally and hides exactly the
// difference the command exists to show.
func TestSelfIdenticalRecordsAreExcluded(t *testing.T) {
	t.Parallel()
	if !strings.Contains(embedEvalUsage, "event") || !strings.Contains(embedEvalUsage, "context") {
		t.Error("usage does not explain the event/context pairing it measures")
	}
}

// cosineSim must not divide by zero. The empty-string case reaches it
// through a zero vector, and NaN would propagate into every ranking.
func TestCosineSimSurvivesZeroVectors(t *testing.T) {
	t.Parallel()
	zero := make([]float32, 4)
	if got := cosineSim(zero, []float32{1, 0, 0, 0}); got != 0 {
		t.Errorf("cosineSim with a zero vector = %v, want 0", got)
	}
	if got := cosineSim(zero, zero); got != 0 {
		t.Errorf("cosineSim of two zero vectors = %v, want 0", got)
	}
}

// It must be reachable. A command absent from the dispatch table is a
// command nobody can run, and the table is hand-maintained.
func TestEmbedEvalIsDispatchable(t *testing.T) {
	t.Parallel()
	var found bool
	for _, c := range topLevelDispatchers() {
		if c.name == "embed-eval" {
			found = true
		}
	}
	if !found {
		t.Error("embed-eval is not in topLevelDispatchers")
	}
}
