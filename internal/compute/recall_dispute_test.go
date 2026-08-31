package compute

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// A disputed memory must arrive with the memory it disagrees with.
//
// Telling a model one of its facts is unreliable, without saying what
// the other side is, leaves it worse off than saying nothing: it now
// knows to distrust the fact and has no way to work out which way.
func TestDisputedRecallCarriesTheOtherSide(t *testing.T) {
	t.Parallel()
	e := NewContextEngine(ContextEngineConfig{})
	entries := []recallEntry{{
		rec:   &lobslawv1.EpisodicRecord{Id: "a", Context: "john is vegetarian", Timestamp: timestamppb.New(time.Now())},
		score: 0.9,
		dispute: &disputeNote{
			verdict:     "conflict",
			counterpart: "john had the steak",
			when:        time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		},
	}}

	got := e.assemble(entries, "test")
	if len(got.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(got.Blocks))
	}
	b := got.Blocks[0]
	if !strings.Contains(b.Content, "john had the steak") {
		t.Errorf("block does not carry the other side:\n%s", b.Content)
	}
	if !strings.Contains(b.Content, "2026-08-01") {
		t.Errorf("block does not date the other side, so 'which is current' is unanswerable:\n%s", b.Content)
	}
	if !strings.Contains(b.Source, "disputed=conflict") {
		t.Errorf("source attribute = %q; want the verdict on it", b.Source)
	}
}

// The pair is one block, so the budget cannot take one half and drop
// the other — which is how a contradiction becomes a confident wrong
// answer.
func TestDisputedPairIsCostedAsOneBlock(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("word ", 200)
	e := NewContextEngine(ContextEngineConfig{MaxRecall: 5, MaxRecallTokens: 60})
	entries := []recallEntry{
		{
			rec:     &lobslawv1.EpisodicRecord{Id: "a", Context: "short fact", Timestamp: timestamppb.New(time.Now())},
			score:   0.9,
			dispute: &disputeNote{verdict: "conflict", counterpart: long},
		},
		{
			rec:   &lobslawv1.EpisodicRecord{Id: "b", Context: "another fact", Timestamp: timestamppb.New(time.Now())},
			score: 0.8,
		},
	}

	got := e.assemble(entries, "test")
	if len(got.Blocks) != 1 {
		t.Fatalf("blocks = %d; the disputed pair's real size was not counted", len(got.Blocks))
	}
	if !strings.Contains(got.Blocks[0].Content, "short fact") {
		t.Errorf("wrong block survived: %q", got.Blocks[0].Content)
	}
}
