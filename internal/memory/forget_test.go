package memory

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestForgetRefusesEmptyQuery(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	_, err := svc.Forget(context.Background(), &lobslawv1.ForgetRequest{})
	if err == nil {
		t.Fatal("expected InvalidArgument for unfiltered forget")
	}
	st, _ := status.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestForgetBySubstring(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	// Store three records; forget should match two of them.
	for _, rec := range []*lobslawv1.VectorRecord{
		{Id: "v-1", Embedding: []float32{1}, Text: "banking PIN is 1234"},
		{Id: "v-2", Embedding: []float32{1}, Text: "grocery list"},
		{Id: "v-3", Embedding: []float32{1}, Text: "bank account number xx1234"},
	} {
		if _, err := svc.Store(ctx, &lobslawv1.StoreRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := svc.Forget(ctx, &lobslawv1.ForgetRequest{Query: "bank"})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if resp.RecordsRemoved != 2 {
		t.Errorf("direct = %d, want 2", resp.RecordsRemoved)
	}

	// v-2 should survive.
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "v-2"}); err != nil {
		t.Errorf("v-2 should still exist: %v", err)
	}
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "v-1"}); err == nil {
		t.Error("v-1 should have been forgotten")
	}
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "v-3"}); err == nil {
		t.Error("v-3 should have been forgotten")
	}
}

func TestForgetBeforeTimestamp(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour)
	newTime := time.Now()

	// Seed records with explicit timestamps.
	for _, rec := range []*lobslawv1.VectorRecord{
		{Id: "old-1", Embedding: []float32{1}, Text: "stale", CreatedAt: timestamppb.New(oldTime)},
		{Id: "new-1", Embedding: []float32{1}, Text: "fresh", CreatedAt: timestamppb.New(newTime)},
	} {
		if _, err := svc.Store(ctx, &lobslawv1.StoreRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	resp, err := svc.Forget(ctx, &lobslawv1.ForgetRequest{
		Before: timestamppb.New(cutoff),
	})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if resp.RecordsRemoved != 1 {
		t.Errorf("direct = %d, want 1", resp.RecordsRemoved)
	}
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "old-1"}); err == nil {
		t.Error("old-1 should have been forgotten")
	}
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "new-1"}); err != nil {
		t.Errorf("new-1 should survive: %v", err)
	}
}

func TestForgetCascadesToConsolidations(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	// Two source records + one consolidated record referencing both.
	for _, rec := range []*lobslawv1.VectorRecord{
		{Id: "src-a", Embedding: []float32{1, 0}, Text: "medical record A"},
		{Id: "src-b", Embedding: []float32{0, 1}, Text: "medical record B"},
		{
			Id:        "consolidated",
			Embedding: []float32{0.7, 0.7},
			Text:      "patient medical summary",
			SourceIds: []string{"src-a", "src-b"},
		},
	} {
		if _, err := svc.Store(ctx, &lobslawv1.StoreRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := svc.Forget(ctx, &lobslawv1.ForgetRequest{Query: "medical record"})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if resp.RecordsRemoved != 2 {
		t.Errorf("direct = %d, want 2", resp.RecordsRemoved)
	}
	if resp.ConsolidationsReforged != 1 {
		t.Errorf("cascaded = %d, want 1", resp.ConsolidationsReforged)
	}

	// Consolidated record must also be gone — aggressive-forget semantics.
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "consolidated"}); err == nil {
		t.Error("consolidated record should have been cascaded away")
	}
}

func TestForgetPartialCascadeStillSweeps(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	// Consolidation references three sources; only one matches the query.
	// Aggressive-forget: consolidation still swept.
	for _, rec := range []*lobslawv1.VectorRecord{
		{Id: "src-a", Embedding: []float32{1, 0, 0}, Text: "bank statement"},
		{Id: "src-b", Embedding: []float32{0, 1, 0}, Text: "grocery receipt"},
		{Id: "src-c", Embedding: []float32{0, 0, 1}, Text: "birthday card"},
		{
			Id:        "consolidated",
			Embedding: []float32{0.5, 0.5, 0.5},
			Text:      "financial summary",
			SourceIds: []string{"src-a", "src-b", "src-c"},
		},
	} {
		if _, err := svc.Store(ctx, &lobslawv1.StoreRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := svc.Forget(ctx, &lobslawv1.ForgetRequest{Query: "bank"})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if resp.RecordsRemoved != 1 {
		t.Errorf("direct = %d, want 1", resp.RecordsRemoved)
	}
	if resp.ConsolidationsReforged != 1 {
		t.Errorf("cascaded = %d, want 1", resp.ConsolidationsReforged)
	}

	// src-b and src-c survive; consolidation swept.
	for _, id := range []string{"src-b", "src-c"} {
		if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: id}); err != nil {
			t.Errorf("%s should survive partial cascade: %v", id, err)
		}
	}
	if _, err := svc.Recall(ctx, &lobslawv1.RecallRequest{Id: "consolidated"}); err == nil {
		t.Error("consolidation should have been swept even on partial source match")
	}
}

func TestForgetByTags(t *testing.T) {
	t.Parallel()
	svc := newTestServiceStack(t)
	ctx := context.Background()

	// Use EpisodicAdd for tag-based matching (VectorRecord uses metadata).
	for _, rec := range []*lobslawv1.EpisodicRecord{
		{Id: "e-1", Event: "saw Dr Smith", Tags: []string{"medical", "appointment"}},
		{Id: "e-2", Event: "made coffee", Tags: []string{"routine"}},
	} {
		if _, err := svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{Record: rec}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := svc.Forget(ctx, &lobslawv1.ForgetRequest{Tags: []string{"medical"}})
	if err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if resp.RecordsRemoved != 1 {
		t.Errorf("direct = %d, want 1", resp.RecordsRemoved)
	}

	// e-2 should survive; VectorRecord.Recall on episodic ids returns NotFound
	// but we can verify via the store.
	if _, err := svc.store.Get(BucketEpisodicRecords, "e-1"); err == nil {
		t.Error("e-1 should have been forgotten")
	}
	if _, err := svc.store.Get(BucketEpisodicRecords, "e-2"); err != nil {
		t.Errorf("e-2 should survive: %v", err)
	}
}
