package memory

import (
	"context"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// TestMergeVerdictStringRoundTrip guards the audit-log / test-assertion
// string form against silent renames. If a verdict's spelling changes,
// existing audit logs become un-parseable and this test fails loudly.
func TestMergeVerdictStringRoundTrip(t *testing.T) {
	t.Parallel()
	cases := map[MergeVerdict]string{
		MergeVerdictKeepDistinct: "keep_distinct",
		MergeVerdictMerge:        "merge",
		MergeVerdictConflict:     "conflict",
		MergeVerdictSupersedes:   "supersedes",
	}
	for v, want := range cases {
		if got := v.String(); got != want {
			t.Errorf("MergeVerdict(%d).String() = %q, want %q", v, got, want)
		}
	}
}

func TestMergeVerdictUnknownString(t *testing.T) {
	t.Parallel()
	if got := MergeVerdict(42).String(); got != "unknown" {
		t.Errorf("unknown verdict should stringify to 'unknown', got %q", got)
	}
}

// TestAlwaysKeepDistinctAdjudicator — the stub must never return
// anything destructive. Boot-safe default: no LLM → no merges.
func TestAlwaysKeepDistinctAdjudicator(t *testing.T) {
	t.Parallel()
	adj := AlwaysKeepDistinctAdjudicator{}
	cluster := &lobslawv1.Cluster{
		Id: "cluster-xyz",
		Records: []*lobslawv1.VectorRecord{
			{Id: "a", Text: "Jen likes cheese"},
			{Id: "b", Text: "Jen enjoys cheese"},
		},
	}
	decision, err := adj.AdjudicateMerge(context.Background(), cluster)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Verdict != MergeVerdictKeepDistinct {
		t.Errorf("stub should always return KeepDistinct; got %s", decision.Verdict)
	}
	if decision.Reason == "" {
		t.Error("Reason should always be populated for audit-log visibility")
	}
	if decision.MergedText != "" {
		t.Error("MergedText must be empty when Verdict != Merge")
	}
}

// TestAlwaysKeepDistinctAdjudicatorSatisfiesInterface — compile-time
// check that the stub can stand in wherever an Adjudicator is needed.
func TestAlwaysKeepDistinctAdjudicatorSatisfiesInterface(t *testing.T) {
	t.Parallel()
	var _ Adjudicator = AlwaysKeepDistinctAdjudicator{}
	var _ Adjudicator = (*AlwaysKeepDistinctAdjudicator)(nil)
}
