package memory

import (
	"math"
	"testing"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// seedVector puts a VectorRecord into the store under the given id.
func seedVector(t *testing.T, s *Store, id string, embedding []float32, scope, retention string) {
	t.Helper()
	rec := &lobslawv1.VectorRecord{
		Id:        id,
		Embedding: embedding,
		Scope:     scope,
		Retention: retention,
	}
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketVectorRecords, id, raw); err != nil {
		t.Fatal(err)
	}
}

func TestVectorSearchRanksBySimilarity(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	// Three vectors: one aligned with query, one orthogonal, one opposite.
	query := []float32{1, 0, 0}
	seedVector(t, s, "aligned", []float32{1, 0, 0}, "", "")
	seedVector(t, s, "orthogonal", []float32{0, 1, 0}, "", "")
	seedVector(t, s, "opposite", []float32{-1, 0, 0}, "", "")

	hits, err := vectorSearch(s, query, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("got %d hits, want 3", len(hits))
	}
	order := []string{hits[0].record.Id, hits[1].record.Id, hits[2].record.Id}
	want := []string{"aligned", "orthogonal", "opposite"}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %q, want %q (full order: %v)", i, order[i], want[i], order)
		}
	}

	if math.Abs(float64(hits[0].score)-1.0) > 1e-5 {
		t.Errorf("aligned score = %v, want ~1.0", hits[0].score)
	}
}

func TestVectorSearchLimit(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	for i := 0; i < 20; i++ {
		id := "v-" + string(rune('a'+i))
		seedVector(t, s, id, []float32{float32(i), 1, 0}, "", "")
	}
	hits, err := vectorSearch(s, []float32{1, 1, 0}, 5, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 {
		t.Errorf("got %d hits, want 5", len(hits))
	}
}

func TestVectorSearchScopeFilter(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedVector(t, s, "public-1", []float32{1, 0, 0}, "public", "episodic")
	seedVector(t, s, "private-1", []float32{1, 0, 0}, "private", "episodic")
	seedVector(t, s, "public-2", []float32{0.9, 0.1, 0}, "public", "episodic")

	hits, err := vectorSearch(s, []float32{1, 0, 0}, 10, "public", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (scope=public only)", len(hits))
	}
	for _, h := range hits {
		if h.record.Scope != "public" {
			t.Errorf("leaked %q (scope=%q)", h.record.Id, h.record.Scope)
		}
	}
}

func TestVectorSearchRetentionFilter(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedVector(t, s, "ep-1", []float32{1, 0, 0}, "", "episodic")
	seedVector(t, s, "lt-1", []float32{1, 0, 0}, "", "long-term")

	hits, err := vectorSearch(s, []float32{1, 0, 0}, 10, "", "long-term")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].record.Id != "lt-1" {
		t.Errorf("unexpected hits: %v", hits)
	}
}

func TestVectorSearchSkipsDimensionMismatch(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	seedVector(t, s, "dim3", []float32{1, 0, 0}, "", "")
	seedVector(t, s, "dim4", []float32{1, 0, 0, 0}, "", "")

	hits, err := vectorSearch(s, []float32{1, 0, 0}, 10, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].record.Id != "dim3" {
		t.Errorf("dimension mismatch not skipped: %v", hits)
	}
}

func TestVectorSearchEmptyQueryRejected(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	if _, err := vectorSearch(s, nil, 10, "", ""); err == nil {
		t.Error("empty query should error")
	}
}

func TestVectorSearchZeroNormQueryRejected(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	if _, err := vectorSearch(s, []float32{0, 0, 0}, 10, "", ""); err == nil {
		t.Error("zero-norm query should error")
	}
}
