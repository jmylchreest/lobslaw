package memory

import (
	"context"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `lobslaw memory forget` has always been a dry run unless --apply,
// and forget is irreversible — it cascades through SourceIds, so "just
// run it and see" is not available. The live form had no dry run at
// all, which would have made the remote command the one that deletes
// on the first try.
//
// Service.Forget also carried its own copy of the matching, while the
// comment on ForgetQuery claimed the CLI and the RPC "cannot diverge
// on what forget these means". These tests hold the shared plan to
// both promises.

func forget(t *testing.T, s *Service, req *lobslawv1.ForgetRequest) *lobslawv1.ForgetResponse {
	t.Helper()
	res, err := s.Forget(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func recordStillThere(t *testing.T, s *Service, id string) bool {
	t.Helper()
	v, e, err := FindRecord(s.store, id)
	if err != nil {
		t.Fatal(err)
	}
	return v != nil || e != nil
}

// A dry run must WRITE NOTHING while still reporting the blast radius.
func TestAForgetDryRunDeletesNothing(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")

	res := forget(t, s, &lobslawv1.ForgetRequest{Ids: []string{"v1"}, DryRun: true})
	if len(res.GetMatched()) != 1 || res.GetMatched()[0] != "v1" {
		t.Errorf("matched = %v; a dry run should still say what it would delete", res.GetMatched())
	}
	if !recordStillThere(t, s, "v1") {
		t.Fatal("a dry run deleted the record")
	}
}

func TestAForgetWithoutDryRunDeletes(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")

	res := forget(t, s, &lobslawv1.ForgetRequest{Ids: []string{"v1"}})
	if res.GetRecordsRemoved() != 1 {
		t.Errorf("records_removed = %d", res.GetRecordsRemoved())
	}
	if recordStillThere(t, s, "v1") {
		t.Error("the record survived a real forget")
	}
}

// A summary whose sources are gone leaks the deleted content through
// its own text and embedding, so the cascade is the point of forget
// rather than a side effect of it.
func TestTheCascadeIsReportedAndApplied(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")
	if _, err := s.Store(context.Background(), &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id: "summary", Owner: "user:alice", Text: "consolidated",
			SourceIds: []string{"v1"}, Embedding: []float32{0.3},
		},
	}); err != nil {
		t.Fatal(err)
	}

	dry := forget(t, s, &lobslawv1.ForgetRequest{Ids: []string{"v1"}, DryRun: true})
	if len(dry.GetSwept()) != 1 || dry.GetSwept()[0] != "summary" {
		t.Fatalf("swept = %v; the dry run does not warn about the cascade", dry.GetSwept())
	}

	forget(t, s, &lobslawv1.ForgetRequest{Ids: []string{"v1"}})
	if recordStillThere(t, s, "summary") {
		t.Error("the consolidation survived; its sources are gone but its text is not")
	}
}

// For a hand-typed id, "no such record" is nearly always a typo, and a
// forget that quietly deletes nothing is the worst outcome.
func TestAMissingIdIsReportedRatherThanIgnored(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")

	res := forget(t, s, &lobslawv1.ForgetRequest{
		Ids: []string{"v1", "ghost"}, DryRun: true,
	})
	if len(res.GetMissing()) != 1 || res.GetMissing()[0] != "ghost" {
		t.Errorf("missing = %v", res.GetMissing())
	}
	if len(res.GetMatched()) != 1 {
		t.Errorf("matched = %v; the real id was lost alongside the typo", res.GetMatched())
	}
}

// An unfiltered forget matches every record in the store.
func TestAForgetWithNoFilterIsStillRefused(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")

	if _, err := s.Forget(context.Background(), &lobslawv1.ForgetRequest{}); err == nil {
		t.Fatal("an unfiltered forget was accepted")
	}
	if !recordStillThere(t, s, "v1") {
		t.Error("an unfiltered forget deleted something")
	}
}

// THE ORDERING THAT MATTERS. A record the requester may not read must
// leave the matched set BEFORE the cascade runs — otherwise it pulls
// its consolidations down with it, deleting through a record the
// caller was never allowed to see.
func TestARecordTheRequesterCannotReadDoesNotCascade(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	ctx := context.Background()

	// bob's private record, and a consolidation built from it.
	if _, err := s.Store(ctx, &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id: "bob-private", Owner: "user:bob", Text: "bob's note",
			Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
			Embedding:  []float32{0.1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store(ctx, &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id: "bob-summary", Owner: "user:bob", Text: "summary of bob's note",
			SourceIds: []string{"bob-private"}, Embedding: []float32{0.2},
			Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
		},
	}); err != nil {
		t.Fatal(err)
	}

	res := forget(t, s, &lobslawv1.ForgetRequest{
		Ids: []string{"bob-private"}, Requester: "user:alice",
	})
	if len(res.GetMatched()) != 0 {
		t.Errorf("matched = %v; alice forgot a record she cannot read", res.GetMatched())
	}
	if len(res.GetSwept()) != 0 {
		t.Errorf("swept = %v; the cascade ran through a record alice cannot read", res.GetSwept())
	}
	if !recordStillThere(t, s, "bob-private") || !recordStillThere(t, s, "bob-summary") {
		t.Error("bob's records were deleted by alice")
	}
}

// The scoping must not block an operator, which is what the empty
// requester is for — and what every CLI invocation uses.
func TestAnEmptyRequesterIsUnrestricted(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	if _, err := s.Store(context.Background(), &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id: "bob-private", Owner: "user:bob", Text: "bob's note",
			Visibility: lobslawv1.Visibility_VISIBILITY_PRIVATE,
			Embedding:  []float32{0.1},
		},
	}); err != nil {
		t.Fatal(err)
	}

	res := forget(t, s, &lobslawv1.ForgetRequest{Ids: []string{"bob-private"}, DryRun: true})
	if len(res.GetMatched()) != 1 {
		t.Errorf("matched = %v; an operator could not reach a private record", res.GetMatched())
	}
}

// The offline plan and the RPC must resolve the same set, or a dry run
// read offline promises something different from what the live one
// does.
func TestTheOfflinePlanAndTheRPCAgree(t *testing.T) {
	t.Parallel()
	s := newTestServiceStack(t)
	putVector(t, s, "v1", "user:alice", "")
	if _, err := s.Store(context.Background(), &lobslawv1.StoreRequest{
		Record: &lobslawv1.VectorRecord{
			Id: "summary", Owner: "user:alice", SourceIds: []string{"v1"},
			Text: "consolidated", Embedding: []float32{0.3},
		},
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanForget(s.store, ForgetQuery{IDs: []string{"v1", "ghost"}})
	if err != nil {
		t.Fatal(err)
	}
	res := forget(t, s, &lobslawv1.ForgetRequest{Ids: []string{"v1", "ghost"}, DryRun: true})

	if len(plan.Matched) != len(res.GetMatched()) ||
		len(plan.Swept) != len(res.GetSwept()) ||
		len(plan.Missing) != len(res.GetMissing()) {
		t.Errorf("offline plan %+v differs from the RPC's %v/%v/%v",
			plan, res.GetMatched(), res.GetSwept(), res.GetMissing())
	}
}
