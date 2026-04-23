package memory

import (
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// seedVectorFull is a richer variant of search_test's seedVector
// that accepts timestamps and source ids so cluster tests can
// exercise time-filter and consolidation-skip paths.
func seedVectorFull(t *testing.T, s *Store, rec *lobslawv1.VectorRecord) {
	t.Helper()
	raw, err := proto.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(BucketVectorRecords, rec.Id, raw); err != nil {
		t.Fatal(err)
	}
}

// TestFindClustersGroupsNearDuplicates is the happy path. Three
// near-aligned vectors (small angle perturbations of [1,0,0]) all
// above the 0.99 cosine threshold should land in one cluster.
func TestFindClustersGroupsNearDuplicates(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	seedVector(t, s, "a", []float32{1.00, 0, 0}, "", "")
	seedVector(t, s, "b", []float32{0.99, 0.01, 0}, "", "")
	seedVector(t, s, "c", []float32{0.98, 0.02, 0}, "", "")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.99})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d: %+v", len(clusters), clusters)
	}
	if len(clusters[0].Records) != 3 {
		t.Errorf("want 3 members, got %d", len(clusters[0].Records))
	}
	if clusters[0].Id == "" {
		t.Error("cluster.Id should be populated (stable hash)")
	}
}

// TestFindClustersDistantVectorsAreDistinctClusters — two pairs of
// near-identical vectors, separated by 90 degrees, should produce
// TWO clusters, not one merged-across-pairs superset.
func TestFindClustersDistantVectorsAreDistinctClusters(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	// Pair 1: aligned with X axis.
	seedVector(t, s, "x1", []float32{1.00, 0, 0}, "", "")
	seedVector(t, s, "x2", []float32{0.99, 0.01, 0}, "", "")

	// Pair 2: aligned with Y axis.
	seedVector(t, s, "y1", []float32{0, 1.00, 0}, "", "")
	seedVector(t, s, "y2", []float32{0.01, 0.99, 0}, "", "")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 {
		t.Fatalf("want 2 clusters (x-pair and y-pair), got %d", len(clusters))
	}
	for _, c := range clusters {
		if len(c.Records) != 2 {
			t.Errorf("each cluster should have 2 members, got %d", len(c.Records))
		}
	}
}

// TestFindClustersSkipsSingletons — one record that's similar to
// nothing else should NOT appear as a 1-element cluster. Enforces
// the min_cluster_size default of 2.
func TestFindClustersSkipsSingletons(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	seedVector(t, s, "loner", []float32{1, 0, 0}, "", "")
	seedVector(t, s, "pair-a", []float32{0, 1, 0}, "", "")
	seedVector(t, s, "pair-b", []float32{0.01, 0.99, 0}, "", "")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster (the pair), got %d", len(clusters))
	}
	for _, r := range clusters[0].Records {
		if r.Id == "loner" {
			t.Error("singleton should not have been clustered")
		}
	}
}

// TestFindClustersSkipsConsolidated — records with SourceIds set
// are summaries, not source records, and shouldn't participate in
// merge detection. The merge flow operates on source records only;
// a consolidation can always be rebuilt from surviving sources.
func TestFindClustersSkipsConsolidated(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	seedVector(t, s, "src-a", []float32{1, 0, 0}, "", "")
	seedVector(t, s, "src-b", []float32{0.99, 0.01, 0}, "", "")

	// A consolidation derived from both sources. Should be skipped.
	seedVectorFull(t, s, &lobslawv1.VectorRecord{
		Id:        "summary",
		Embedding: []float32{0.995, 0.005, 0},
		SourceIds: []string{"src-a", "src-b"},
	})

	clusters, err := findClusters(s, clusterQuery{threshold: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster (sources only), got %d", len(clusters))
	}
	ids := []string{clusters[0].Records[0].Id, clusters[0].Records[1].Id}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"src-a", "src-b"}) {
		t.Errorf("cluster should contain only sources; got %v", ids)
	}
}

// TestFindClustersRetentionFilter — session-retention records
// shouldn't participate in long-term merge clustering. Critical for
// keeping ephemeral chatter out of consolidated memory.
func TestFindClustersRetentionFilter(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	seedVector(t, s, "lt-a", []float32{1, 0, 0}, "", "long-term")
	seedVector(t, s, "lt-b", []float32{0.99, 0.01, 0}, "", "long-term")
	seedVector(t, s, "sess-a", []float32{0.98, 0.02, 0}, "", "session")

	clusters, err := findClusters(s, clusterQuery{
		threshold:       0.95,
		retentionFilter: "long-term",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(clusters))
	}
	if len(clusters[0].Records) != 2 {
		t.Errorf("cluster should exclude session record; got %d members", len(clusters[0].Records))
	}
	for _, r := range clusters[0].Records {
		if r.Id == "sess-a" {
			t.Error("session record leaked into long-term cluster")
		}
	}
}

// TestFindClustersScopeFilter is the scope counterpart to retention
// filtering. Records in a different scope must not cluster with
// the target scope.
func TestFindClustersScopeFilter(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	seedVector(t, s, "mine-a", []float32{1, 0, 0}, "alice", "")
	seedVector(t, s, "mine-b", []float32{0.99, 0.01, 0}, "alice", "")
	seedVector(t, s, "bob", []float32{0.98, 0.02, 0}, "bob", "")

	clusters, err := findClusters(s, clusterQuery{
		threshold:   0.95,
		scopeFilter: "alice",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(clusters))
	}
	for _, r := range clusters[0].Records {
		if r.Scope != "alice" {
			t.Errorf("scope filter leaked; got record with scope %q", r.Scope)
		}
	}
}

// TestFindClustersBeforeFilter — records newer than the `before`
// cutoff don't participate. Lets operators run "merge anything
// older than 30 days" Dream passes.
func TestFindClustersBeforeFilter(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	now := time.Now()
	old := timestamppb.New(now.Add(-48 * time.Hour))
	recent := timestamppb.New(now.Add(-1 * time.Hour))

	seedVectorFull(t, s, &lobslawv1.VectorRecord{Id: "old-a", Embedding: []float32{1, 0, 0}, CreatedAt: old})
	seedVectorFull(t, s, &lobslawv1.VectorRecord{Id: "old-b", Embedding: []float32{0.99, 0.01, 0}, CreatedAt: old})
	seedVectorFull(t, s, &lobslawv1.VectorRecord{Id: "recent", Embedding: []float32{0.98, 0.02, 0}, CreatedAt: recent})

	clusters, err := findClusters(s, clusterQuery{
		threshold: 0.95,
		before:    now.Add(-24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster of old records, got %d", len(clusters))
	}
	for _, r := range clusters[0].Records {
		if r.Id == "recent" {
			t.Error("recent record survived the before-filter")
		}
	}
}

// TestFindClustersSkipsMixedDimensions — a store with vectors from
// two different embedding models shouldn't cross-cluster them.
// Dimension mismatch is a valid state (model upgrade in progress)
// and must not crash the scan.
func TestFindClustersSkipsMixedDimensions(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	seedVector(t, s, "d3-a", []float32{1, 0, 0}, "", "")
	seedVector(t, s, "d3-b", []float32{0.99, 0.01, 0}, "", "")
	seedVector(t, s, "d4", []float32{1, 0, 0, 0}, "", "")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.95})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster (the d3 pair), got %d", len(clusters))
	}
	for _, r := range clusters[0].Records {
		if len(r.Embedding) != 3 {
			t.Errorf("cluster mixed dimensions; got dim=%d", len(r.Embedding))
		}
	}
}

// TestFindClustersSimilarityStats checks MinSimilarity and
// AvgSimilarity populate correctly for a 3-cluster with non-
// trivially-different pairwise distances.
func TestFindClustersSimilarityStats(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	// Tightly spaced: pairwise similarity ≥ 0.99, < 1.0.
	seedVector(t, s, "a", []float32{1.00, 0, 0}, "", "")
	seedVector(t, s, "b", []float32{0.99, 0.01, 0}, "", "")
	seedVector(t, s, "c", []float32{0.98, 0.02, 0}, "", "")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.99})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 1 {
		t.Fatalf("want 1 cluster, got %d", len(clusters))
	}
	c := clusters[0]
	if c.MinSimilarity < 0.99 || c.MinSimilarity > 1.0 {
		t.Errorf("MinSimilarity out of expected band: %f", c.MinSimilarity)
	}
	if c.AvgSimilarity < c.MinSimilarity {
		t.Errorf("AvgSimilarity %f should be >= MinSimilarity %f", c.AvgSimilarity, c.MinSimilarity)
	}
}

// TestFindClustersStableID — same input two runs, same cluster ID.
// Important because the Dream runner uses cluster.Id to correlate
// re-observations across runs (audit log "cluster X seen again").
func TestFindClustersStableID(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	seedVector(t, s, "a", []float32{1, 0, 0}, "", "")
	seedVector(t, s, "b", []float32{0.99, 0.01, 0}, "", "")

	c1, _ := findClusters(s, clusterQuery{threshold: 0.95})
	c2, _ := findClusters(s, clusterQuery{threshold: 0.95})

	if c1[0].Id != c2[0].Id {
		t.Errorf("cluster ID must be stable across runs; got %q vs %q", c1[0].Id, c2[0].Id)
	}
}

// TestFindClustersEmptyStoreReturnsNone — graceful no-op on a
// freshly-opened store. Operators wiring up FindClusters to boot
// shouldn't crash on day one.
func TestFindClustersEmptyStoreReturnsNone(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	clusters, err := findClusters(s, clusterQuery{threshold: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 0 {
		t.Errorf("empty store should yield no clusters; got %d", len(clusters))
	}
}

// TestFindClustersChunkingSplitsHairballs — when everything is
// transitively similar to everything else (a "hairball"), chunking
// splits into smaller groups. Validates the max_cluster_size bound.
func TestFindClustersChunkingSplitsHairballs(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)

	// Seven near-aligned vectors, maxClusterSize=3 → at least 2-3 chunks.
	seedVector(t, s, "v1", []float32{1, 0, 0}, "", "")
	seedVector(t, s, "v2", []float32{0.999, 0.001, 0}, "", "")
	seedVector(t, s, "v3", []float32{0.998, 0.002, 0}, "", "")
	seedVector(t, s, "v4", []float32{0.997, 0.003, 0}, "", "")
	seedVector(t, s, "v5", []float32{0.996, 0.004, 0}, "", "")
	seedVector(t, s, "v6", []float32{0.995, 0.005, 0}, "", "")
	seedVector(t, s, "v7", []float32{0.994, 0.006, 0}, "", "")

	clusters, err := findClusters(s, clusterQuery{threshold: 0.99, maxClusterSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) < 2 {
		t.Fatalf("maxClusterSize=3 with 7 members should produce multiple chunks; got %d", len(clusters))
	}
	for _, c := range clusters {
		if len(c.Records) > 3 {
			t.Errorf("cluster exceeded maxClusterSize=3; got %d members", len(c.Records))
		}
	}
}
