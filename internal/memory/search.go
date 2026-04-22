package memory

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// searchHit is an internal result from vector search before it's
// marshalled back to the gRPC response.
type searchHit struct {
	record *lobslawv1.VectorRecord
	score  float32
}

// vectorSearch linearly scans the vector bucket, computes cosine
// similarity against query, and returns the top-K most similar records.
// Scope/retention filters run inline — records failing any filter are
// skipped without scoring.
//
// Cost is O(N × D) where N is the record count and D is the embedding
// dimension. Fine for personal scale (< ~100k records). Post-MVP we
// can swap in HNSW or similar — tracked in DEFERRED.md.
func vectorSearch(store *Store, query []float32, limit int, scopeFilter, retentionFilter string) ([]searchHit, error) {
	if len(query) == 0 {
		return nil, errors.New("search query embedding is empty")
	}
	if limit <= 0 {
		limit = 10
	}
	queryNorm := norm(query)
	if queryNorm == 0 {
		return nil, errors.New("search query embedding has zero norm")
	}

	var hits []searchHit
	err := store.ForEach(BucketVectorRecords, func(_ string, value []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(value, &v); err != nil {
			return fmt.Errorf("unmarshal vector record: %w", err)
		}
		if scopeFilter != "" && v.Scope != scopeFilter {
			return nil
		}
		if retentionFilter != "" && v.Retention != retentionFilter {
			return nil
		}
		if len(v.Embedding) != len(query) {
			// Dimension mismatch — skip rather than fail; a heterogeneous
			// store (mixed embedding models) is a valid state.
			return nil
		}
		candNorm := norm(v.Embedding)
		if candNorm == 0 {
			return nil
		}
		score := dot(query, v.Embedding) / (queryNorm * candNorm)
		hits = append(hits, searchHit{record: &v, score: score})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// dot computes the dot product of two equal-length float32 slices.
// Assumes len(a) == len(b) — caller is responsible.
func dot(a, b []float32) float32 {
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// norm returns the L2 norm of v. Returns 0 for an empty or zero vector.
func norm(v []float32) float32 {
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	return float32(math.Sqrt(float64(sum)))
}
