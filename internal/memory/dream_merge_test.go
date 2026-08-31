package memory

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// stubAdjudicator returns a fixed verdict and counts the asking.
type stubAdjudicator struct {
	verdict *Adjudication
	err     error
	calls   int
}

func (s *stubAdjudicator) AdjudicateMerge(_ context.Context, _ *lobslawv1.Cluster) (*Adjudication, error) {
	s.calls++
	return s.verdict, s.err
}

// seedPair writes a memory the way Remember does: an episodic record
// plus the vector that indexes it.
func seedPair(t *testing.T, s *Store, id, owner, text string, vec []float32, ts time.Time) {
	t.Helper()
	seedEpisodic(t, s, &lobslawv1.EpisodicRecord{
		Id:         id,
		Owner:      owner,
		Event:      text,
		Context:    text,
		Importance: 5,
		Timestamp:  timestamppb.New(ts),
		Retention:  lobslawv1.Retention_RETENTION_LONG_TERM,
		Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
	})
	v := &lobslawv1.VectorRecord{
		Id:         "vec-" + id,
		Embedding:  vec,
		Text:       text,
		Scope:      "episodic",
		Retention:  lobslawv1.Retention_RETENTION_LONG_TERM,
		SourceIds:  []string{id},
		Owner:      owner,
		Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		CreatedAt:  timestamppb.New(ts),
	}
	raw, err := proto.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketVectorRecords, v.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func mergeRunner(t *testing.T, svc *Service, adj Adjudicator) *DreamRunner {
	t.Helper()
	d := NewDreamRunner(svc.raft, svc.store, nil, DreamConfig{Now: fixedNow}, nil)
	if adj != nil {
		d.SetAdjudicator(adj, stubEmbedder{model: "stub"})
	}
	return d
}

// Without an adjudicator the phase must not even cluster. The
// previous implementation installed a stub that always kept records
// distinct, so it paid for an O(n²) similarity pass every night to
// reach a conclusion it had before it started.
func TestMergePhaseDoesNothingWithoutAnAdjudicator(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "user:test", "john lives in leeds", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "user:test", "john's home is leeds", []float32{1, 0, 0}, fixedNow())

	d := mergeRunner(t, svc, nil)
	out, err := d.mergePhase(context.Background(), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if out.Clusters != 0 {
		t.Errorf("clustered %d groups with no adjudicator; the phase should be absent", out.Clusters)
	}
}

// A merge replaces the sources with one memory and forgets them.
func TestMergePhaseConsolidatesDuplicates(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "user:test", "john lives in leeds", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "user:test", "john's home is leeds", []float32{1, 0, 0}, fixedNow())

	adj := &stubAdjudicator{verdict: &Adjudication{
		Verdict:      VerdictMerge,
		Reason:       "the same fact twice",
		Consolidated: "John lives in Leeds.",
	}}
	d := mergeRunner(t, svc, adj)

	out, err := d.mergePhase(context.Background(), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if out.Merged != 1 {
		t.Fatalf("merged %d clusters, want 1 (clusters=%d)", out.Merged, out.Clusters)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := svc.store.Get(BucketEpisodicRecords, id); err == nil {
			t.Errorf("source %q survived the merge — the duplicate is still in recall", id)
		}
	}

	log, err := ListConsolidations(svc.store, ConsolidationQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0].Verdict != string(VerdictMerge) {
		t.Fatalf("consolidation log = %+v; want one merge verdict", log)
	}
	// The replacement has to be readable and owned, or the merge has
	// destroyed two memories to write one nobody can see.
	raw, err := svc.store.Get(BucketEpisodicRecords, log[0].ResultId)
	if err != nil {
		t.Fatalf("merged memory is not in the store: %v", err)
	}
	var merged lobslawv1.EpisodicRecord
	if err := proto.Unmarshal(raw, &merged); err != nil {
		t.Fatal(err)
	}
	if merged.Owner != "user:test" {
		t.Errorf("merged memory owner = %q; an unowned merge is a memory nobody can read", merged.Owner)
	}
	if merged.Visibility != lobslawv1.Visibility_VISIBILITY_PRIVATE {
		t.Errorf("merged visibility = %v; a summary of private records is private", merged.Visibility)
	}
}

// A conflict keeps both sides and leaves a trail recall can follow.
func TestMergePhaseMarksConflictsWithoutResolvingThem(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "user:test", "john is vegetarian", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "user:test", "john had the steak", []float32{1, 0, 0}, fixedNow())

	adj := &stubAdjudicator{verdict: &Adjudication{
		Verdict: VerdictConflict,
		Reason:  "Are you vegetarian, or was the steak the exception?",
	}}
	d := mergeRunner(t, svc, adj)

	out, err := d.mergePhase(context.Background(), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if out.Conflicts != 1 {
		t.Fatalf("conflicts = %d, want 1", out.Conflicts)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := svc.store.Get(BucketEpisodicRecords, id); err != nil {
			t.Errorf("record %q was removed by a conflict verdict; nothing may be resolved automatically", id)
		}
	}
	// The index is the half the old design never had: a verdict
	// nothing can look up is a verdict nothing acts on.
	disputes, err := DisputesFor(svc.store, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(disputes) != 1 || disputes[0].Verdict != string(VerdictConflict) {
		t.Fatalf("DisputesFor(a) = %+v; want the conflict verdict", disputes)
	}
	if got := CounterpartsOf(disputes[0], "a"); len(got) != 1 || got[0] != "b" {
		t.Errorf("counterparts of a = %v; want [b]", got)
	}
}

// The same cluster must not be re-adjudicated every night. Cluster
// ids hash their members, so unchanged membership is the same
// question — and asking it nightly forever is a standing bill.
func TestMergePhaseSkipsClustersAlreadyDecided(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "user:test", "john plays guitar", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "user:test", "john is learning guitar", []float32{1, 0, 0}, fixedNow())

	adj := &stubAdjudicator{verdict: &Adjudication{
		Verdict: VerdictKeepDistinct,
		Reason:  "different claims about the same instrument",
	}}
	d := mergeRunner(t, svc, adj)

	if _, err := d.mergePhase(context.Background(), fixedNow()); err != nil {
		t.Fatal(err)
	}
	out, err := d.mergePhase(context.Background(), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if adj.calls != 1 {
		t.Errorf("adjudicator asked %d times about the same cluster; want 1", adj.calls)
	}
	if out.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", out.Skipped)
	}
}

// A merge verdict with no text would delete records in favour of
// nothing. Downgraded, not obeyed — and still recorded, so the
// cluster is not asked about again.
func TestMergePhaseRefusesAnEmptyConsolidation(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "user:test", "john lives in leeds", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "user:test", "john's home is leeds", []float32{1, 0, 0}, fixedNow())

	adj := &stubAdjudicator{verdict: &Adjudication{Verdict: VerdictMerge, Reason: "same"}}
	d := mergeRunner(t, svc, adj)

	out, err := d.mergePhase(context.Background(), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if out.Merged != 0 || out.Distinct != 1 {
		t.Errorf("merged=%d distinct=%d; an empty consolidation must not delete anything", out.Merged, out.Distinct)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := svc.store.Get(BucketEpisodicRecords, id); err != nil {
			t.Errorf("record %q was deleted for an empty consolidation", id)
		}
	}
}

// An unresolved conflict is a question for the person whose memories
// these are; one whose sides no longer both exist is not.
func TestNightmaresAreLiveConflictsOnly(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "user:test", "john is vegetarian", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "user:test", "john had the steak", []float32{1, 0, 0}, fixedNow())

	adj := &stubAdjudicator{verdict: &Adjudication{
		Verdict: VerdictConflict,
		Reason:  "Are you vegetarian, or was the steak the exception?",
	}}
	d := mergeRunner(t, svc, adj)
	if _, err := d.mergePhase(context.Background(), fixedNow()); err != nil {
		t.Fatal(err)
	}

	got, err := UnresolvedNightmares(svc.store, "user:test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Question == "" {
		t.Fatalf("nightmares = %+v; want one question", got)
	}

	// Answering it by forgetting one side settles it, with no
	// resolved flag to keep in step.
	if err := svc.store.Delete(BucketEpisodicRecords, "b"); err != nil {
		t.Fatal(err)
	}
	got, err = UnresolvedNightmares(svc.store, "user:test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("still asking about a conflict with one side left: %+v", got)
	}
}

// Another principal's contradictions are not this one's business:
// the question quotes the memories.
func TestNightmaresAreOwnerScoped(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "user:alice", "alice is vegetarian", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "user:alice", "alice had the steak", []float32{1, 0, 0}, fixedNow())

	adj := &stubAdjudicator{verdict: &Adjudication{Verdict: VerdictConflict, Reason: "which is it?"}}
	d := mergeRunner(t, svc, adj)
	if _, err := d.mergePhase(context.Background(), fixedNow()); err != nil {
		t.Fatal(err)
	}

	got, err := UnresolvedNightmares(svc.store, "user:bob", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("bob was told about alice's contradictions: %+v", got)
	}
}

// Records written before ownership existed all share the empty
// owner, so they cluster with each other. Adjudicating them costs a
// model call whose verdict cannot be recorded — a merge refuses for
// want of an owner, before the verdict is written — so the same
// cluster would be sent again every night forever.
func TestMergePhaseLeavesUnownedClustersAlone(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	seedPair(t, svc.store, "a", "", "legacy record one", []float32{1, 0, 0}, fixedNow())
	seedPair(t, svc.store, "b", "", "legacy record two", []float32{1, 0, 0}, fixedNow())

	adj := &stubAdjudicator{verdict: &Adjudication{Verdict: VerdictKeepDistinct}}
	d := mergeRunner(t, svc, adj)

	out, err := d.mergePhase(context.Background(), fixedNow())
	if err != nil {
		t.Fatal(err)
	}
	if adj.calls != 0 {
		t.Errorf("adjudicator asked %d times about an unowned cluster; the verdict could not be recorded either way", adj.calls)
	}
	if out.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", out.Skipped)
	}
}
