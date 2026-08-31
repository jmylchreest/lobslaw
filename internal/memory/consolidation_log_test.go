package memory

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Memory that silently rewrites itself and cannot be inspected is a
// trust problem. These are about the record being complete enough to
// answer the two questions a user actually asks: "why did it merge
// those" and "why did it NOT merge those".

// A consolidation log that leaked across owners would describe one
// person's memories to another.
func TestListScopesByOwner(t *testing.T) {
	t.Parallel()
	store := freshStore(t)
	seedConsolidation(t, store, "a", "user:alice", "merge", time.Now())
	seedConsolidation(t, store, "b", "user:bob", "merge", time.Now())

	got, err := ListConsolidations(store, ConsolidationQuery{Owner: "user:alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Owner != "user:alice" {
		t.Errorf("owner filter returned %+v", got)
	}
}

func TestListFiltersAndOrders(t *testing.T) {
	t.Parallel()
	store := freshStore(t)
	now := time.Now()
	seedConsolidation(t, store, "old", "user:alice", "merge", now.Add(-72*time.Hour))
	seedConsolidation(t, store, "mid", "user:alice", "keep_distinct", now.Add(-2*time.Hour))
	seedConsolidation(t, store, "new", "user:alice", "merge", now)

	all, err := ListConsolidations(store, ConsolidationQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Id != "new" || all[2].Id != "old" {
		t.Errorf("not newest-first: %v", recordIDs(all))
	}

	recent, err := ListConsolidations(store, ConsolidationQuery{Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Errorf("since filter returned %v", recordIDs(recent))
	}

	merges, err := ListConsolidations(store, ConsolidationQuery{Verdict: "merge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(merges) != 2 {
		t.Errorf("verdict filter returned %v", recordIDs(merges))
	}

	limited, err := ListConsolidations(store, ConsolidationQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Id != "new" {
		t.Errorf("limit returned %v; it should keep the newest", recordIDs(limited))
	}
}

func recordIDs(recs []*lobslawv1.ConsolidationRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Id)
	}
	return out
}

// --- helpers -------------------------------------------------------

func freshStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	return s
}

func seedConsolidation(t *testing.T, s *Store, id, owner, verdict string, at time.Time) {
	t.Helper()
	rec := &lobslawv1.ConsolidationRecord{
		Id: id, ClusterId: id, Owner: owner, Verdict: verdict,
		CreatedAt: timestamppb.New(at),
	}
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketConsolidations, id, raw); err != nil {
		t.Fatal(err)
	}
}
