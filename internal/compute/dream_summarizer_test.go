package compute

import (
	"context"
	"strings"
	"testing"
)

// Dream has scored, pruned and digested since it was written and
// never once consolidated: the step is gated on a Summarizer and
// nothing outside tests ever set one. On a cluster with 157 episodic
// records that produced "candidates=10 consolidated=0" nightly, which
// reads as a working pass finding nothing worth merging.

func summarizerSaying(t *testing.T, reply string) *DreamSummarizer {
	t.Helper()
	return NewDreamSummarizer(NewMockProvider(MockResponse{Content: reply}), "m", nil)
}

func TestEpisodesBecomeAConsolidation(t *testing.T) {
	t.Parallel()
	s := summarizerSaying(t, "John prefers British spelling and works on lobslaw.")
	got, emb, err := s.Summarize(context.Background(),
		[]string{"asked for British spelling", "worked on lobslaw"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "British spelling") {
		t.Errorf("summary = %q", got)
	}
	// No embedder is involved: Dream's candidate selection is recency
	// times importance and touches no vector. A nil embedding leaves
	// the summary findable lexically, which is what memory_search
	// already falls back to.
	if emb != nil {
		t.Errorf("an embedding appeared from nowhere: %v", emb)
	}
}

// "NOTHING" is a documented answer, not a failure — Dream skips the
// write on an empty summary, so a pass over episodes that amount to
// nothing records nothing rather than a paragraph saying so.
func TestNothingWorthKeepingWritesNothing(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{"NOTHING", "nothing", " NOTHING. ", "**NOTHING**"} {
		got, _, err := summarizerSaying(t, reply).Summarize(context.Background(), []string{"hi"})
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("reply %q produced a summary: %q", reply, got)
		}
	}
}

// A SUBSTRING TEST GETS THIS WRONG. A consolidation can legitimately
// contain the word — "nothing came of the London trip" is a summary,
// not a refusal, and discarding it loses the memory silently.
func TestASummaryMentioningNothingIsStillASummary(t *testing.T) {
	t.Parallel()
	const real = "Nothing came of the London trip; John cancelled it in March."
	got, _, err := summarizerSaying(t, real).Summarize(context.Background(), []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if got != real {
		t.Errorf("a real summary was discarded as a refusal: %q", got)
	}
}

// An unbounded set of episodes is an unbounded prompt, and the cost
// of a nightly pass should not scale with how much happened that day.
func TestTheEpisodeCountIsBounded(t *testing.T) {
	t.Parallel()
	provider := NewMockProvider(MockResponse{Content: "summary"})
	s := NewDreamSummarizer(provider, "m", nil)

	events := make([]string, maxDreamEvents+50)
	for i := range events {
		events[i] = "episode"
	}
	if _, _, err := s.Summarize(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	sent := provider.Calls()
	if len(sent) != 1 {
		t.Fatalf("made %d requests", len(sent))
	}
	body := sent[0].Messages[len(sent[0].Messages)-1].Content
	if n := strings.Count(body, "episode"); n != maxDreamEvents {
		t.Errorf("sent %d episodes, want the cap of %d", n, maxDreamEvents)
	}
}

// Nothing to consolidate is not an error, and neither is a node with
// no provider — Dream keeps scoring and pruning, as it always did.
func TestAnEmptyOrAbsentInputIsNotAnError(t *testing.T) {
	t.Parallel()
	s := summarizerSaying(t, "unused")
	for _, events := range [][]string{nil, {}, {"", "   "}} {
		got, _, err := s.Summarize(context.Background(), events)
		if err != nil || got != "" {
			t.Errorf("events %v -> %q, %v", events, got, err)
		}
	}
	if NewDreamSummarizer(nil, "m", nil) != nil {
		t.Error("a nil provider produced a summarizer")
	}
}
