package memory

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// fixedAdjudicator returns a preset MergeDecision for every cluster,
// optionally with an error. Used to exercise each verdict path
// deterministically without an LLM.
type fixedAdjudicator struct {
	decision MergeDecision
	err      error
}

func (f fixedAdjudicator) AdjudicateMerge(_ context.Context, _ *lobslawv1.Cluster) (MergeDecision, error) {
	return f.decision, f.err
}

// seedLongTermPair inserts two near-identical long-term VectorRecords
// so FindClusters finds exactly one cluster. Helper for merge tests.
func seedLongTermPair(t *testing.T, s *Store, idA, idB string) {
	t.Helper()
	seedVectorFull(t, s, &lobslawv1.VectorRecord{
		Id:        idA,
		Embedding: []float32{1.00, 0, 0},
		Text:      "Jen likes cheese",
		Retention: string(types.RetentionLongTerm),
	})
	seedVectorFull(t, s, &lobslawv1.VectorRecord{
		Id:        idB,
		Embedding: []float32{0.99, 0.01, 0},
		Text:      "Jen enjoys cheese",
		Retention: string(types.RetentionLongTerm),
	})
}

// TestMergePhaseMergeVerdict — the destructive happy path:
// Adjudicator says "merge", mergePhase must store one consolidated
// record and delete the originals via Raft.
func TestMergePhaseMergeVerdict(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := svc.store
	d := NewDreamRunner(svc.raft, s, nil, DreamConfig{}, nil)
	d.SetAdjudicator(fixedAdjudicator{decision: MergeDecision{
		Verdict:    MergeVerdictMerge,
		MergedText: "Jen enjoys cheese (merged)",
		Reason:     "test",
	}})

	seedLongTermPair(t, s, "a", "b")

	result, err := d.mergePhase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Merged != 1 {
		t.Errorf("want Merged=1, got %d", result.Merged)
	}

	// Originals should be gone.
	if raw, _ := s.Get(BucketVectorRecords, "a"); raw != nil {
		t.Error("source 'a' should have been deleted by merge")
	}
	if raw, _ := s.Get(BucketVectorRecords, "b"); raw != nil {
		t.Error("source 'b' should have been deleted by merge")
	}

	// Consolidated record should exist with both sources in SourceIds.
	var foundConsolidated bool
	_ = s.ForEach(BucketVectorRecords, func(id string, value []byte) error {
		var v lobslawv1.VectorRecord
		_ = proto.Unmarshal(value, &v)
		if len(v.SourceIds) == 2 && v.Text == "Jen enjoys cheese (merged)" {
			foundConsolidated = true
		}
		return nil
	})
	if !foundConsolidated {
		t.Error("consolidated record not found after merge")
	}
}

// TestMergePhaseKeepDistinctIsNoOp — the safe default verdict must
// leave every record untouched. Covers the "boot-default stub"
// path (AlwaysKeepDistinctAdjudicator) plus any future explicit
// keep-distinct verdicts from a real LLM.
func TestMergePhaseKeepDistinctIsNoOp(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := svc.store
	d := NewDreamRunner(svc.raft, s, nil, DreamConfig{}, nil)
	// Default stub; no SetAdjudicator needed.

	seedLongTermPair(t, s, "a", "b")

	result, err := d.mergePhase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Merged != 0 || result.Conflicts != 0 || result.Supersedes != 0 {
		t.Errorf("default stub should produce no actions; got %+v", result)
	}
	if raw, _ := s.Get(BucketVectorRecords, "a"); raw == nil {
		t.Error("source 'a' should survive keep-distinct")
	}
	if raw, _ := s.Get(BucketVectorRecords, "b"); raw == nil {
		t.Error("source 'b' should survive keep-distinct")
	}
}

// TestMergePhaseConflictTagsMembers — conflict verdict tags every
// cluster member with conflict-cluster:<id> in metadata, preserving
// all records.
func TestMergePhaseConflictTagsMembers(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := svc.store
	d := NewDreamRunner(svc.raft, s, nil, DreamConfig{}, nil)
	d.SetAdjudicator(fixedAdjudicator{decision: MergeDecision{
		Verdict: MergeVerdictConflict,
		Reason:  "contradicting assertions",
	}})

	seedLongTermPair(t, s, "a", "b")

	result, err := d.mergePhase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Conflicts != 1 {
		t.Errorf("want Conflicts=1, got %d", result.Conflicts)
	}
	for _, id := range []string{"a", "b"} {
		raw, _ := s.Get(BucketVectorRecords, id)
		if raw == nil {
			t.Errorf("record %q should survive conflict verdict", id)
			continue
		}
		var v lobslawv1.VectorRecord
		_ = proto.Unmarshal(raw, &v)
		if v.Metadata["conflict-cluster"] == "" {
			t.Errorf("record %q should have conflict-cluster tag; got metadata %v", id, v.Metadata)
		}
	}
}

// TestMergePhaseSupersedesTagsMembers — supersedes tags parallel
// to conflict, different key.
func TestMergePhaseSupersedesTagsMembers(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := svc.store
	d := NewDreamRunner(svc.raft, s, nil, DreamConfig{}, nil)
	d.SetAdjudicator(fixedAdjudicator{decision: MergeDecision{
		Verdict: MergeVerdictSupersedes,
		Reason:  "newer state",
	}})

	seedLongTermPair(t, s, "a", "b")

	result, err := d.mergePhase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Supersedes != 1 {
		t.Errorf("want Supersedes=1, got %d", result.Supersedes)
	}
	for _, id := range []string{"a", "b"} {
		raw, _ := s.Get(BucketVectorRecords, id)
		var v lobslawv1.VectorRecord
		_ = proto.Unmarshal(raw, &v)
		if v.Metadata["supersedes-chain"] == "" {
			t.Errorf("record %q should have supersedes-chain tag", id)
		}
	}
}

// TestMergePhaseAdjudicatorErrorIsConservative — if the Adjudicator
// returns an error, the cluster is left intact (no data lost).
// This is the non-negotiable safety invariant: LLM failure must
// NEVER be destructive.
func TestMergePhaseAdjudicatorErrorIsConservative(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := svc.store
	d := NewDreamRunner(svc.raft, s, nil, DreamConfig{}, nil)
	d.SetAdjudicator(fixedAdjudicator{err: fmt.Errorf("LLM timeout")})

	seedLongTermPair(t, s, "a", "b")

	result, err := d.mergePhase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Error → no action taken on this cluster.
	if result.Merged != 0 || result.Conflicts != 0 || result.Supersedes != 0 {
		t.Errorf("SECURITY: adjudicator error must not trigger actions; got %+v", result)
	}
	if raw, _ := s.Get(BucketVectorRecords, "a"); raw == nil {
		t.Error("source 'a' should survive adjudicator error")
	}
	if raw, _ := s.Get(BucketVectorRecords, "b"); raw == nil {
		t.Error("source 'b' should survive adjudicator error")
	}
}

// TestMergePhaseOnlyLongTermRecords — session/episodic records
// must NOT participate in merge. Enforces the retention-filter
// plumbing end-to-end.
func TestMergePhaseOnlyLongTermRecords(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	s := svc.store
	d := NewDreamRunner(svc.raft, s, nil, DreamConfig{}, nil)
	d.SetAdjudicator(fixedAdjudicator{decision: MergeDecision{
		Verdict:    MergeVerdictMerge,
		MergedText: "merged",
		Reason:     "test",
	}})

	// Two near-identical SESSION records — should be ignored entirely.
	seedVectorFull(t, s, &lobslawv1.VectorRecord{
		Id: "sess-a", Embedding: []float32{1, 0, 0}, Retention: string(types.RetentionSession),
	})
	seedVectorFull(t, s, &lobslawv1.VectorRecord{
		Id: "sess-b", Embedding: []float32{0.99, 0.01, 0}, Retention: string(types.RetentionSession),
	})

	result, err := d.mergePhase(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Merged != 0 {
		t.Errorf("session records should never be merged; got Merged=%d", result.Merged)
	}
	if raw, _ := s.Get(BucketVectorRecords, "sess-a"); raw == nil {
		t.Error("session record 'sess-a' should survive (not merge-eligible)")
	}
}

// TestComputeCentroid checks the centroid calculation: element-wise
// mean of same-dim embeddings. Mixed-dim inputs are skipped (not
// averaged) so the result dim matches records[0]'s embedding.
func TestComputeCentroid(t *testing.T) {
	t.Parallel()
	records := []*lobslawv1.VectorRecord{
		{Embedding: []float32{1.0, 0.0, 0.0}},
		{Embedding: []float32{0.0, 1.0, 0.0}},
		{Embedding: []float32{0.0, 0.0, 1.0}},
	}
	got := computeCentroid(records)
	want := []float32{1.0 / 3, 1.0 / 3, 1.0 / 3}
	for i, v := range got {
		if v < want[i]-1e-6 || v > want[i]+1e-6 {
			t.Errorf("centroid[%d] = %f, want ~%f", i, v, want[i])
		}
	}
}

func TestComputeCentroidEmpty(t *testing.T) {
	t.Parallel()
	if got := computeCentroid(nil); got != nil {
		t.Errorf("nil input should return nil; got %v", got)
	}
	if got := computeCentroid([]*lobslawv1.VectorRecord{{Embedding: nil}}); got != nil {
		t.Errorf("empty-embedding input should return nil; got %v", got)
	}
}

func TestComputeCentroidSkipsMixedDim(t *testing.T) {
	t.Parallel()
	records := []*lobslawv1.VectorRecord{
		{Embedding: []float32{1, 0, 0}},
		{Embedding: []float32{0, 1}}, // different dim — skipped
		{Embedding: []float32{0, 0, 1}},
	}
	got := computeCentroid(records)
	if len(got) != 3 {
		t.Errorf("centroid dim = %d, want 3 (first record's dim)", len(got))
	}
	// With the mid record skipped, centroid should be avg of {1,0,0} and {0,0,1} = {0.5, 0, 0.5}.
	expect := []float32{0.5, 0, 0.5}
	for i, v := range got {
		if v < expect[i]-1e-6 || v > expect[i]+1e-6 {
			t.Errorf("centroid[%d] = %f, want %f", i, v, expect[i])
		}
	}
}
