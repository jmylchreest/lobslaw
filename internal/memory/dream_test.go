package memory

import (
	"context"
	"math"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// stubSummarizer records the summarize call count for test assertions
// and returns a fixed summary + embedding.
type stubSummarizer struct {
	summary   string
	embedding []float32
	err       error
	calls     int
}

func (s *stubSummarizer) Summarize(_ context.Context, _ []string) (string, []float32, error) {
	s.calls++
	return s.summary, s.embedding, s.err
}

// fixedNow returns a deterministic now() for score/decay calculations.
func fixedNow() time.Time {
	return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
}

// seedEpisodic inserts an EpisodicRecord into the store.
func seedEpisodic(t *testing.T, s *Store, rec *lobslawv1.EpisodicRecord) {
	t.Helper()
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketEpisodicRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestDreamScoreAllSkipsConsolidated(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	now := fixedNow()

	// Source record.
	seedEpisodic(t, s, &lobslawv1.EpisodicRecord{
		Id:         "src-1",
		Importance: 8,
		Timestamp:  timestamppb.New(now.Add(-24 * time.Hour)),
	})
	// Consolidated record (has SourceIds).
	seedEpisodic(t, s, &lobslawv1.EpisodicRecord{
		Id:         "cons-1",
		Importance: 9,
		Timestamp:  timestamppb.New(now),
		SourceIds:  []string{"src-1"},
	})

	d := &DreamRunner{
		store:  s,
		cfg:    DreamConfig{HalfLife: 14 * 24 * time.Hour, Now: func() time.Time { return now }},
		logger: nil,
	}
	scored, err := d.scoreAll(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(scored) != 1 {
		t.Fatalf("want 1 scored record (consolidated skipped), got %d", len(scored))
	}
	if scored[0].id != "src-1" {
		t.Errorf("unexpected record scored: %q", scored[0].id)
	}
}

func TestDreamScoreRecencyDecay(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	now := fixedNow()
	halfLife := 14 * 24 * time.Hour

	// Two records: one fresh, one one half-life old. Same importance.
	seedEpisodic(t, s, &lobslawv1.EpisodicRecord{
		Id: "fresh", Importance: 5,
		Timestamp: timestamppb.New(now),
	})
	seedEpisodic(t, s, &lobslawv1.EpisodicRecord{
		Id: "half-life-old", Importance: 5,
		Timestamp: timestamppb.New(now.Add(-halfLife)),
	})

	d := &DreamRunner{store: s, cfg: DreamConfig{HalfLife: halfLife, Now: func() time.Time { return now }}}
	scored, err := d.scoreAll(now)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]float32{}
	for _, r := range scored {
		byID[r.id] = r.score
	}
	// fresh should be ~5; half-life-old should be ~2.5.
	if math.Abs(float64(byID["fresh"]-5)) > 0.01 {
		t.Errorf("fresh score = %v, want ~5", byID["fresh"])
	}
	if math.Abs(float64(byID["half-life-old"]-2.5)) > 0.01 {
		t.Errorf("half-life-old score = %v, want ~2.5", byID["half-life-old"])
	}
}

func TestDreamSelectTopN(t *testing.T) {
	t.Parallel()
	d := &DreamRunner{}
	input := []scoredRecord{
		{id: "a", score: 1.0},
		{id: "b", score: 9.0},
		{id: "c", score: 5.0},
		{id: "d", score: 3.0},
	}
	got := d.selectTopN(input, 2)
	if len(got) != 2 {
		t.Fatalf("want 2 selected, got %d", len(got))
	}
	if got[0].id != "b" || got[1].id != "c" {
		t.Errorf("order = [%q, %q], want [b, c]", got[0].id, got[1].id)
	}
}

func TestDreamPrunePreservesLongTerm(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	old := fixedNow().Add(-90 * 24 * time.Hour) // very low recency decay

	// Below-threshold episodic with default retention (should be pruned).
	_, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{
		Record: &lobslawv1.EpisodicRecord{
			Id:         "prune-me",
			Event:      "old grocery list",
			Importance: 1, // score stays tiny after decay
			Timestamp:  timestamppb.New(old),
			Retention:  "episodic",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Below-threshold LONG-TERM retention (must survive).
	_, err = svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{
		Record: &lobslawv1.EpisodicRecord{
			Id:         "keep-me",
			Event:      "user's wedding anniversary",
			Importance: 1,
			Timestamp:  timestamppb.New(old),
			Retention:  "long-term",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	d := NewDreamRunner(svc.raft, svc.store, nil, DreamConfig{
		PruneThreshold: 0.1,
		HalfLife:       14 * 24 * time.Hour,
		Now:            func() time.Time { return fixedNow() },
	}, nil)

	result, err := d.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Pruned < 1 {
		t.Errorf("want at least 1 pruned, got %d", result.Pruned)
	}

	// keep-me must still exist.
	if _, err := svc.store.Get(BucketEpisodicRecords, "keep-me"); err != nil {
		t.Errorf("long-term record should survive prune: %v", err)
	}
	// prune-me must be gone.
	if _, err := svc.store.Get(BucketEpisodicRecords, "prune-me"); err == nil {
		t.Error("prune-me should have been pruned")
	}
}

func TestDreamRunWithSummarizer(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()
	now := fixedNow()

	// Two high-score episodics.
	for _, rec := range []*lobslawv1.EpisodicRecord{
		{Id: "e-1", Event: "met alice", Importance: 9, Timestamp: timestamppb.New(now)},
		{Id: "e-2", Event: "met bob", Importance: 8, Timestamp: timestamppb.New(now)},
	} {
		if _, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	sum := &stubSummarizer{summary: "met alice and bob", embedding: []float32{0.1, 0.2, 0.3}}
	d := NewDreamRunner(svc.raft, svc.store, sum, DreamConfig{
		MaxCandidates:  5,
		PruneThreshold: 0.1,
		Now:            func() time.Time { return now },
	}, nil)

	result, err := d.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sum.calls != 1 {
		t.Errorf("summarizer calls = %d, want 1", sum.calls)
	}
	if result.Consolidated != 1 {
		t.Errorf("Consolidated = %d, want 1", result.Consolidated)
	}

	// Consolidated record should exist in VectorRecord bucket with the
	// correct SourceIds.
	var found bool
	err = svc.store.ForEach(BucketVectorRecords, func(id string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return err
		}
		if len(v.SourceIds) == 2 && v.Text == "met alice and bob" {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("consolidated VectorRecord not found")
	}
}

func TestDreamRunWithoutSummarizerSkipsConsolidation(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	_, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{
		Record: &lobslawv1.EpisodicRecord{
			Id: "e-1", Event: "hello", Importance: 9,
			Timestamp: timestamppb.New(fixedNow()),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	d := NewDreamRunner(svc.raft, svc.store, nil, DreamConfig{Now: func() time.Time { return fixedNow() }}, nil)
	result, err := d.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Consolidated != 0 {
		t.Errorf("Consolidated = %d, want 0 (no summarizer)", result.Consolidated)
	}
	if len(result.Candidates) == 0 {
		t.Error("candidates should still be populated even without summarizer")
	}
}

func TestServiceDreamViaGRPC(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	// Wire a stub summarizer.
	sum := &stubSummarizer{summary: "consolidated", embedding: []float32{0.1}}
	svc.DreamRunner().SetSummarizer(sum)
	svc.DreamRunner().cfg.Now = func() time.Time { return fixedNow() }

	// Seed one candidate.
	_, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{
		Record: &lobslawv1.EpisodicRecord{
			Id: "e-1", Event: "interesting event", Importance: 9,
			Timestamp: timestamppb.New(fixedNow()),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := svc.Dream(ctx, &lobslawv1.DreamRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Consolidated != 1 {
		t.Errorf("response.Consolidated = %d, want 1", resp.Consolidated)
	}
	if sum.calls != 1 {
		t.Errorf("summarizer calls = %d, want 1", sum.calls)
	}
}

func TestServiceDreamUnimplementedWithoutRaft(t *testing.T) {
	t.Parallel()
	svc := &Service{store: nil, raft: nil, dreamRunner: nil}
	_, err := svc.Dream(context.Background(), &lobslawv1.DreamRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", st.Code())
	}
}
