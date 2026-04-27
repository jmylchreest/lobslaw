package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Default bounds for FindClusters callers who leave the request
// fields at zero. Chosen to match the behaviour described in
// docs/dev/MEMORY.md's "Dream-time merge flow" section.
const (
	defaultClusterThreshold = 0.88
	defaultMinClusterSize   = 2
	defaultMaxClusterSize   = 10
)

// clusterEdge is a file-scope named type so findClusters,
// buildCluster, and chunkCluster can all hand [] of the same type
// around. Anonymous struct literals of identical fields are
// distinct types in Go; a named type avoids the friction.
type clusterEdge struct {
	a, b       int
	similarity float32
}

// findClusters runs pairwise cosine similarity over the vector bucket
// and returns connected components where every edge exceeds threshold.
//
// Shape: O(n²) in embedding count — a hard upper bound on cost per
// Dream run. Acceptable at personal scale (< ~100k records); a
// future HNSW-backed variant is tracked in DEFERRED.md for anything
// bigger. Runs off the hot path (Dream only), so SIMD tuning isn't
// on the critical path either.
//
// Scope/retention/before filters run during the initial candidate
// scan so we never compare records the caller wouldn't accept as
// cluster members anyway.
//
// Giant connected components (everything transitively similar to
// everything else — a "hairball") are split by nearest-first
// chunking once they exceed maxClusterSize. This preserves O(n)
// output bounds while keeping the highest-similarity pairs grouped.
func findClusters(store *Store, req clusterQuery) ([]*lobslawv1.Cluster, error) {
	if req.threshold <= 0 {
		req.threshold = defaultClusterThreshold
	}
	if req.threshold > 1 {
		return nil, errors.New("threshold must be in (0, 1]")
	}
	if req.minClusterSize < 2 {
		req.minClusterSize = defaultMinClusterSize
	}
	if req.maxClusterSize < req.minClusterSize {
		req.maxClusterSize = defaultMaxClusterSize
		if req.maxClusterSize < req.minClusterSize {
			req.maxClusterSize = req.minClusterSize
		}
	}

	candidates, err := scanClusterCandidates(store, req)
	if err != nil {
		return nil, err
	}
	if len(candidates) < 2 {
		return nil, nil
	}

	// Cache L2 norms so the pairwise loop doesn't recompute them.
	norms := make([]float32, len(candidates))
	for i, c := range candidates {
		norms[i] = norm(c.record.Embedding)
	}

	uf := newUnionFind(len(candidates))
	edges := make([]clusterEdge, 0)

	for i := range candidates {
		if norms[i] == 0 {
			continue
		}
		if len(candidates[i].record.Embedding) == 0 {
			continue
		}
		for j := i + 1; j < len(candidates); j++ {
			if norms[j] == 0 {
				continue
			}
			if len(candidates[i].record.Embedding) != len(candidates[j].record.Embedding) {
				// Mixed embedding dimensions — two records from different
				// embedding models can never be "near-duplicates."
				continue
			}
			sim := dot(candidates[i].record.Embedding, candidates[j].record.Embedding) / (norms[i] * norms[j])
			if sim >= req.threshold {
				uf.union(i, j)
				edges = append(edges, clusterEdge{a: i, b: j, similarity: sim})
			}
		}
	}

	// Group members by their union-find root.
	groups := make(map[int][]int)
	for i := range candidates {
		root := uf.find(i)
		groups[root] = append(groups[root], i)
	}

	// Materialise clusters, dropping anything below min size. For
	// anything above max size, split into chunks of (maxClusterSize)
	// taken highest-similarity-pair-first.
	var clusters []*lobslawv1.Cluster
	for _, members := range groups {
		if len(members) < req.minClusterSize {
			continue
		}
		chunks := chunkCluster(members, edges, req.maxClusterSize)
		for _, chunk := range chunks {
			c := buildCluster(candidates, chunk, edges, norms)
			if c != nil {
				clusters = append(clusters, c)
			}
		}
	}

	// Deterministic output — sort by descending avg similarity so
	// tighter clusters come first (operators reading a report see
	// the strongest signals up top).
	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].AvgSimilarity > clusters[j].AvgSimilarity
	})
	if req.limit > 0 && len(clusters) > req.limit {
		clusters = clusters[:req.limit]
	}
	return clusters, nil
}

// clusterQuery is the internal shape of a FindClusters call after
// defaults are resolved. Keeps findClusters's signature narrow and
// makes the grpc-handler → internal-helper seam obvious.
type clusterQuery struct {
	threshold       float32
	minClusterSize  int
	maxClusterSize  int
	scopeFilter     string
	retentionFilter lobslawv1.Retention
	before          time.Time
	limit           int
}

// clusterCandidate carries a record + its bucket-local index so
// the pairwise loop can reference members by int without holding
// the record bytes in a map.
type clusterCandidate struct {
	id     string
	record *lobslawv1.VectorRecord
}

// scanClusterCandidates walks the vector bucket once, applying all
// filters inline, and returns the records eligible for pairwise
// comparison. Consolidated records (SourceIDs non-empty) are skipped
// — we cluster source records, not summaries.
func scanClusterCandidates(store *Store, req clusterQuery) ([]clusterCandidate, error) {
	var out []clusterCandidate
	err := store.ForEach(BucketVectorRecords, func(id string, value []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(value, &v); err != nil {
			return fmt.Errorf("unmarshal vector %q: %w", id, err)
		}
		if len(v.SourceIds) > 0 {
			return nil
		}
		if req.scopeFilter != "" && v.Scope != req.scopeFilter {
			return nil
		}
		if req.retentionFilter != lobslawv1.Retention_RETENTION_UNSPECIFIED && v.Retention != req.retentionFilter {
			return nil
		}
		if !req.before.IsZero() && v.CreatedAt != nil && !v.CreatedAt.AsTime().Before(req.before) {
			return nil
		}
		if len(v.Embedding) == 0 {
			return nil
		}
		out = append(out, clusterCandidate{id: id, record: &v})
		return nil
	})
	return out, err
}

// buildCluster fills in the returned Cluster message — copies the
// records, computes min/avg similarity across the edges that belong
// to this cluster, stamps a stable id (hash of sorted member IDs).
func buildCluster(
	candidates []clusterCandidate,
	members []int,
	edges []clusterEdge,
	_ []float32,
) *lobslawv1.Cluster {
	if len(members) < 2 {
		return nil
	}
	memberSet := make(map[int]struct{}, len(members))
	for _, m := range members {
		memberSet[m] = struct{}{}
	}

	minSim := float32(1.0)
	var sumSim float32
	var count int
	for _, e := range edges {
		_, aIn := memberSet[e.a]
		_, bIn := memberSet[e.b]
		if !aIn || !bIn {
			continue
		}
		if e.similarity < minSim {
			minSim = e.similarity
		}
		sumSim += e.similarity
		count++
	}
	if count == 0 {
		return nil
	}
	avgSim := sumSim / float32(count)

	records := make([]*lobslawv1.VectorRecord, 0, len(members))
	ids := make([]string, 0, len(members))
	for _, m := range members {
		records = append(records, candidates[m].record)
		ids = append(ids, candidates[m].id)
	}
	sort.Strings(ids)

	return &lobslawv1.Cluster{
		Id:            clusterIDFor(ids),
		Records:       records,
		MinSimilarity: minSim,
		AvgSimilarity: avgSim,
	}
}

// clusterIDFor produces a stable identifier for a cluster from its
// sorted member IDs. Same cluster (same membership) across runs →
// same ID, so audit logs can correlate re-observations.
func clusterIDFor(sortedIDs []string) string {
	h := sha256.New()
	for _, id := range sortedIDs {
		h.Write([]byte(id))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return "cluster-" + hex.EncodeToString(sum[:8])
}

// chunkCluster splits a connected component exceeding maxSize into
// smaller chunks. Greedy: repeatedly pull the highest-similarity
// edge whose endpoints haven't been chunked yet, grow a chunk until
// it hits maxSize, move on. Preserves the "tightest neighbours stay
// together" property that a hairball would otherwise violate.
//
// For components at or below maxSize this is a no-op (returns the
// input wrapped in one chunk).
func chunkCluster(members []int, edges []clusterEdge, maxSize int) [][]int {
	if len(members) <= maxSize {
		return [][]int{members}
	}
	memberSet := make(map[int]struct{}, len(members))
	for _, m := range members {
		memberSet[m] = struct{}{}
	}
	// Restrict edges to those whose both endpoints live in this cluster.
	var local []clusterEdge
	for _, e := range edges {
		if _, ok := memberSet[e.a]; !ok {
			continue
		}
		if _, ok := memberSet[e.b]; !ok {
			continue
		}
		local = append(local, e)
	}
	sort.Slice(local, func(i, j int) bool {
		return local[i].similarity > local[j].similarity
	})

	placed := make(map[int]int) // member → chunk index
	var chunks [][]int
	for _, e := range local {
		_, aPlaced := placed[e.a]
		_, bPlaced := placed[e.b]
		switch {
		case !aPlaced && !bPlaced:
			idx := len(chunks)
			chunks = append(chunks, []int{e.a, e.b})
			placed[e.a] = idx
			placed[e.b] = idx
		case aPlaced && !bPlaced:
			idx := placed[e.a]
			if len(chunks[idx]) < maxSize {
				chunks[idx] = append(chunks[idx], e.b)
				placed[e.b] = idx
			}
		case !aPlaced && bPlaced:
			idx := placed[e.b]
			if len(chunks[idx]) < maxSize {
				chunks[idx] = append(chunks[idx], e.a)
				placed[e.a] = idx
			}
		}
	}
	// Any unplaced members (if the edge set didn't cover them because
	// they only link to already-full chunks) go into a final leftover
	// chunk so we don't silently drop records.
	var orphans []int
	for _, m := range members {
		if _, ok := placed[m]; !ok {
			orphans = append(orphans, m)
		}
	}
	for len(orphans) > 0 {
		take := len(orphans)
		if take > maxSize {
			take = maxSize
		}
		chunks = append(chunks, orphans[:take])
		orphans = orphans[take:]
	}
	return chunks
}

// unionFind is the textbook path-compression + union-by-rank
// implementation. Used by findClusters to aggregate threshold-
// linked records into connected components. Package-local because
// the implementation doesn't have any callers outside this file.
type unionFind struct {
	parent []int
	rank   []int
}

func newUnionFind(n int) *unionFind {
	u := &unionFind{parent: make([]int, n), rank: make([]int, n)}
	for i := range u.parent {
		u.parent[i] = i
	}
	return u
}

func (u *unionFind) find(x int) int {
	if u.parent[x] != x {
		u.parent[x] = u.find(u.parent[x])
	}
	return u.parent[x]
}

func (u *unionFind) union(a, b int) {
	ra, rb := u.find(a), u.find(b)
	if ra == rb {
		return
	}
	if u.rank[ra] < u.rank[rb] {
		u.parent[ra] = rb
	} else if u.rank[ra] > u.rank[rb] {
		u.parent[rb] = ra
	} else {
		u.parent[rb] = ra
		u.rank[ra]++
	}
}
