package memory

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestSessionPruneDropsExpiredSessionRecords(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	now := time.Now()
	old := timestamppb.New(now.Add(-48 * time.Hour))
	recent := timestamppb.New(now.Add(-30 * time.Minute))

	for _, rec := range []*lobslawv1.EpisodicRecord{
		{Id: "ep-stale-session", Event: "ephemeral", Timestamp: old, Retention: string(types.RetentionSession)},
		{Id: "ep-fresh-session", Event: "still useful", Timestamp: recent, Retention: string(types.RetentionSession)},
		{Id: "ep-stale-episodic", Event: "keep me", Timestamp: old, Retention: string(types.RetentionEpisodic)},
	} {
		if _, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}
	for _, rec := range []*lobslawv1.VectorRecord{
		{Id: "vec-stale-session", Embedding: []float32{1}, Text: "draft", Retention: string(types.RetentionSession), CreatedAt: old},
		{Id: "vec-stale-long", Embedding: []float32{1}, Text: "permanent", Retention: string(types.RetentionLongTerm), CreatedAt: old},
	} {
		if _, err := svc.Store(ctx, &lobslawv1.StoreRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	pruner := svc.SessionPruner()
	if pruner == nil {
		t.Fatal("session pruner not initialised")
	}
	pruner.cfg.MaxAge = 24 * time.Hour
	pruner.cfg.Now = func() time.Time { return now }

	result, err := pruner.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.EpisodicPruned != 1 {
		t.Errorf("episodic pruned = %d, want 1", result.EpisodicPruned)
	}
	if result.VectorPruned != 1 {
		t.Errorf("vector pruned = %d, want 1", result.VectorPruned)
	}

	// Stale session records gone.
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "vec-stale-session"}); err == nil {
		t.Error("vec-stale-session should have been pruned")
	}
	// Fresh session record + non-session retention survive.
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "vec-stale-long"}); err != nil {
		t.Errorf("vec-stale-long (long-term) should survive: %v", err)
	}
}

func TestSessionPruneIgnoresMissingTimestamps(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	if _, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{Record: &lobslawv1.EpisodicRecord{
		Id:        "ep-no-ts",
		Event:     "no timestamp",
		Retention: string(types.RetentionSession),
	}}); err != nil {
		t.Fatal(err)
	}

	pruner := svc.SessionPruner()
	pruner.cfg.MaxAge = time.Hour
	result, err := pruner.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.EpisodicPruned != 0 {
		t.Errorf("expected nothing pruned without timestamp, got %d", result.EpisodicPruned)
	}
}
