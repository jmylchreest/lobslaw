package memory

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
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
			Retention:  lobslawv1.Retention_RETENTION_EPISODIC,
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
			Retention:  lobslawv1.Retention_RETENTION_LONG_TERM,
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
	// correct SourceIds. Sources were both episodic (default), so the
	// consolidation should inherit episodic retention — NOT long-term.
	var found bool
	var gotRetention string
	err = svc.store.ForEach(BucketVectorRecords, func(id string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return err
		}
		if len(v.SourceIds) == 2 && v.Text == "met alice and bob" {
			found = true
			gotRetention = types.RetentionString(v.Retention)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Error("consolidated VectorRecord not found")
	}
	if gotRetention != "episodic" {
		t.Errorf("consolidation retention = %q, want episodic (inherited from sources)", gotRetention)
	}
}

func TestDreamConsolidationInheritsHighestRetention(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()
	now := fixedNow()

	// Mix of retentions: one long-term, two episodic. Consolidation
	// should inherit long-term (the highest).
	for _, rec := range []*lobslawv1.EpisodicRecord{
		{Id: "e-1", Event: "routine A", Importance: 9, Timestamp: timestamppb.New(now), Retention: lobslawv1.Retention_RETENTION_EPISODIC},
		{Id: "e-2", Event: "anniversary", Importance: 9, Timestamp: timestamppb.New(now), Retention: lobslawv1.Retention_RETENTION_LONG_TERM},
		{Id: "e-3", Event: "routine B", Importance: 9, Timestamp: timestamppb.New(now), Retention: lobslawv1.Retention_RETENTION_EPISODIC},
	} {
		if _, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	sum := &stubSummarizer{summary: "mixed-retention summary", embedding: []float32{0.1}}
	d := NewDreamRunner(svc.raft, svc.store, sum, DreamConfig{Now: func() time.Time { return now }}, nil)
	if _, err := d.Run(ctx); err != nil {
		t.Fatal(err)
	}

	var gotRetention string
	err := svc.store.ForEach(BucketVectorRecords, func(_ string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return err
		}
		if v.Text == "mixed-retention summary" {
			gotRetention = types.RetentionString(v.Retention)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotRetention != "long-term" {
		t.Errorf("consolidation retention = %q, want long-term (highest among sources)", gotRetention)
	}
}

func TestHighestRetention(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []lobslawv1.Retention
		want lobslawv1.Retention
	}{
		{"empty", nil, lobslawv1.Retention_RETENTION_UNSPECIFIED},
		{"only session", []lobslawv1.Retention{lobslawv1.Retention_RETENTION_SESSION}, lobslawv1.Retention_RETENTION_SESSION},
		{"only episodic", []lobslawv1.Retention{lobslawv1.Retention_RETENTION_EPISODIC}, lobslawv1.Retention_RETENTION_EPISODIC},
		{"only long-term", []lobslawv1.Retention{lobslawv1.Retention_RETENTION_LONG_TERM}, lobslawv1.Retention_RETENTION_LONG_TERM},
		{"session + episodic", []lobslawv1.Retention{lobslawv1.Retention_RETENTION_SESSION, lobslawv1.Retention_RETENTION_EPISODIC}, lobslawv1.Retention_RETENTION_EPISODIC},
		{"episodic + long-term", []lobslawv1.Retention{lobslawv1.Retention_RETENTION_EPISODIC, lobslawv1.Retention_RETENTION_LONG_TERM}, lobslawv1.Retention_RETENTION_LONG_TERM},
		{"all three", []lobslawv1.Retention{lobslawv1.Retention_RETENTION_SESSION, lobslawv1.Retention_RETENTION_EPISODIC, lobslawv1.Retention_RETENTION_LONG_TERM}, lobslawv1.Retention_RETENTION_LONG_TERM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := highestRetention(tt.in); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
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

// seedCommitment writes an AgentCommitment directly into the
// commitments bucket so digest tests don't have to plumb through
// the scheduler.
func seedCommitment(t *testing.T, s *Store, c *lobslawv1.AgentCommitment) {
	t.Helper()
	raw, err := proto.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketCommitments, c.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestDreamDigestRollsUpFiredCommitments(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()
	now := fixedNow()

	// Two done commitments on day-1, one on day-2, all past the
	// 24h grace.
	day1 := now.Add(-48 * time.Hour)
	day2 := now.Add(-72 * time.Hour)
	seedCommitment(t, svc.store, &lobslawv1.AgentCommitment{
		Id: "c-day1-a", Status: "done", DueAt: timestamppb.New(day1),
		Reason: "weather check", Params: map[string]string{"prompt": "fetch weather"},
	})
	seedCommitment(t, svc.store, &lobslawv1.AgentCommitment{
		Id: "c-day1-b", Status: "done", DueAt: timestamppb.New(day1.Add(2 * time.Hour)),
		Reason: "Iris's leaving drinks", Params: map[string]string{"prompt": "wish her well"},
	})
	seedCommitment(t, svc.store, &lobslawv1.AgentCommitment{
		Id: "c-day2-a", Status: "done", DueAt: timestamppb.New(day2),
		Reason: "go to bed", Params: map[string]string{"prompt": "remind to sleep"},
	})

	d := NewDreamRunner(svc.raft, svc.store, nil, DreamConfig{
		Now: func() time.Time { return now },
	}, nil)
	result, err := d.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommitmentsDigested != 3 {
		t.Errorf("CommitmentsDigested = %d; want 3", result.CommitmentsDigested)
	}
	if result.CommitmentDigests != 2 {
		t.Errorf("CommitmentDigests = %d; want 2 (one per day)", result.CommitmentDigests)
	}

	// Originals must be gone.
	for _, id := range []string{"c-day1-a", "c-day1-b", "c-day2-a"} {
		if _, err := svc.store.Get(BucketCommitments, id); err == nil {
			t.Errorf("commitment %q should be deleted", id)
		}
	}

	digestsFound := 0
	combinedBodies := ""
	err = svc.store.ForEach(BucketEpisodicRecords, func(_ string, raw []byte) error {
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		hasDigestTag := false
		for _, tag := range rec.Tags {
			if tag == "commitment-digest" {
				hasDigestTag = true
			}
		}
		if !hasDigestTag {
			return nil
		}
		digestsFound++
		if rec.Retention != lobslawv1.Retention_RETENTION_LONG_TERM {
			t.Errorf("digest %s retention = %v; want LONG_TERM", rec.Id, rec.Retention)
		}
		combinedBodies += rec.Context
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if digestsFound != 2 {
		t.Errorf("found %d commitment-digest records; want 2", digestsFound)
	}
	for _, want := range []string{"weather check", "Iris's leaving drinks", "go to bed"} {
		if !strings.Contains(combinedBodies, want) {
			t.Errorf("digest bodies missing %q; combined = %q", want, combinedBodies)
		}
	}
}

func TestDreamDigestRespectsGraceWindow(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()
	now := fixedNow()

	// Fired 1 hour ago — inside the 24h grace, should be left alone.
	seedCommitment(t, svc.store, &lobslawv1.AgentCommitment{
		Id: "c-fresh", Status: "done", DueAt: timestamppb.New(now.Add(-time.Hour)),
		Reason: "fresh delivery",
	})
	// Pending — never digested regardless of age.
	seedCommitment(t, svc.store, &lobslawv1.AgentCommitment{
		Id: "c-pending", Status: "pending", DueAt: timestamppb.New(now.Add(-100 * time.Hour)),
		Reason: "still queued",
	})

	d := NewDreamRunner(svc.raft, svc.store, nil, DreamConfig{
		Now: func() time.Time { return now },
	}, nil)
	result, err := d.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.CommitmentsDigested != 0 {
		t.Errorf("CommitmentsDigested = %d; want 0 (grace + pending should both be skipped)", result.CommitmentsDigested)
	}

	for _, id := range []string{"c-fresh", "c-pending"} {
		if _, err := svc.store.Get(BucketCommitments, id); err != nil {
			t.Errorf("commitment %q should still exist; err = %v", id, err)
		}
	}
}

