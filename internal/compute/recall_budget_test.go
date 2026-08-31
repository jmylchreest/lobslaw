package compute

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// assembleWith runs the block-building half of recall against records
// the test supplies, so the budget can be exercised without a store.
func assembleWith(t *testing.T, maxRecall, maxTokens int, recs ...*lobslawv1.EpisodicRecord) ContextAssembly {
	t.Helper()
	e := NewContextEngine(ContextEngineConfig{MaxRecall: maxRecall, MaxRecallTokens: maxTokens})
	entries := make([]recallEntry, 0, len(recs))
	for i, r := range recs {
		entries = append(entries, recallEntry{rec: r, score: float32(len(recs) - i)})
	}
	return e.assemble(entries, "test")
}

func rec(id, text string) *lobslawv1.EpisodicRecord {
	return &lobslawv1.EpisodicRecord{Id: id, Context: text, Timestamp: timestamppb.New(time.Now())}
}

// Recall was bounded only by cardinality, so three records was
// anywhere from a line to most of a page. The count is what an
// operator reasons about; the size is what the context window charges
// for, and both now apply.
func TestRecallStopsAtTheTighterOfTheTwoBounds(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("a fact worth remembering. ", 40) // ~1000 chars

	t.Run("the size bound cuts a long set short", func(t *testing.T) {
		t.Parallel()
		got := assembleWith(t, 5, 200, rec("1", long), rec("2", long), rec("3", long))
		if len(got.Blocks) >= 3 {
			t.Errorf("blocks = %d; the size bound did not fire on records of ~%d chars",
				len(got.Blocks), len(long))
		}
	})

	t.Run("the count bound still applies to short records", func(t *testing.T) {
		t.Parallel()
		got := assembleWith(t, 2, 10000, rec("1", "short"), rec("2", "short"), rec("3", "short"))
		if len(got.Blocks) != 2 {
			t.Errorf("blocks = %d, want 2 — the count bound was not applied", len(got.Blocks))
		}
	})
}

// One record over budget is still admitted when nothing has been taken
// yet. Recalling a single long memory beats recalling none and leaving
// the turn to guess — a budget that can return empty is a budget that
// silently disables recall for anyone whose memories are verbose.
func TestASingleOversizedRecordIsStillRecalled(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("x", 5000)
	got := assembleWith(t, 5, 10, rec("1", huge))
	if len(got.Blocks) != 1 {
		t.Errorf("blocks = %d, want 1; a record larger than the whole budget was dropped "+
			"rather than admitted alone", len(got.Blocks))
	}
}

// The ids reported back must be the ones actually rendered, or a
// caller citing them describes memories the model never saw.
func TestReportedIDsMatchWhatWasRendered(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("b", 900)
	got := assembleWith(t, 5, 250, rec("1", long), rec("2", long), rec("3", long))
	if len(got.RecallIDs) != len(got.Blocks) {
		t.Errorf("RecallIDs = %d but blocks = %d; the citation list and the prompt disagree",
			len(got.RecallIDs), len(got.Blocks))
	}
}
