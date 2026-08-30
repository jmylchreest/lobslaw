package memory

import (
	"fmt"
	"math"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Benchmarks for the vector recall hot path.
//
// vectorSearch is on every turn's critical path via compute.ContextEngine,
// so its cost is paid by every user message whether or not recall helps.
// The function's own comment claims "Fine for personal scale (< ~100k
// records)" — these benchmarks exist so that claim is a measurement rather
// than an assertion, and so the effect of any optimisation is visible
// rather than argued.
//
// The scan does four things per record, and the layered benchmarks below
// separate them so effort lands where the time actually goes:
//
//	1. crypto.Open       — secretbox decrypt of the whole record  (Store.ForEach)
//	2. proto.Unmarshal   — including VectorRecord.Text, which scoring never reads
//	3. norm(v.Embedding) — recomputed every query for every record
//	4. dot + compare     — the only arithmetic the algorithm actually needs
//
// Run:
//
//	go test ./internal/memory -run '^$' -bench 'Vector|Cosine|Norm|Scan' -benchmem
//
// Compare a change against a baseline:
//
//	go test ./internal/memory -run '^$' -bench Vector -benchmem -count=6 > new.txt
//	benchstat old.txt new.txt

// benchDim is a realistic embedding width. text-embedding-3-small is 1536;
// nomic-embed-text and bge-* are 768. 1536 is the pessimistic common case.
const benchDim = 1536

// benchStore opens a real sealed bbolt store — not a fake — because
// decryption is one of the costs under measurement. Using an in-memory
// map here would hide the thing most worth knowing.
func benchStore(b *testing.B) *Store {
	b.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		b.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(b.TempDir(), "state.db"), key)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

// randVec returns a deterministic pseudo-random unit-ish vector. Fixed
// seed per index so a benchmark run is reproducible and two runs are
// comparable.
func randVec(rng *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

// seedVectors writes n VectorRecords with realistic text payloads. The
// text matters: it is unmarshalled on every query and then discarded,
// so a benchmark with empty text would understate the real cost.
func seedVectors(b *testing.B, s *Store, n, dim int) {
	b.Helper()
	rng := rand.New(rand.NewSource(42))
	text := make([]byte, 600) // ~a turn's worth of user message + reply
	for i := range text {
		text[i] = byte('a' + i%26)
	}
	for i := range n {
		rec := &lobslawv1.VectorRecord{
			Id:        fmt.Sprintf("vec-%08d", i),
			Embedding: randVec(rng, dim),
			Text:      string(text),
			Scope:     "episodic",
			Retention: lobslawv1.Retention_RETENTION_LONG_TERM,
		}
		raw, err := proto.Marshal(rec)
		if err != nil {
			b.Fatal(err)
		}
		if err := s.Put(BucketVectorRecords, rec.Id, raw); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkVectorSearch measures the full hot path at several corpus
// sizes. The interesting number is ns/op against N: the scan is linear,
// so this establishes the per-record constant that determines where
// "personal scale" actually stops being acceptable for a per-turn cost.
func BenchmarkVectorSearch(b *testing.B) {
	for _, n := range []int{100, 1_000, 10_000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			s := benchStore(b)
			seedVectors(b, s, n, benchDim)
			q := randVec(rand.New(rand.NewSource(7)), benchDim)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				hits, err := vectorSearch(s, q, 3, Everyone(), "",
					lobslawv1.Retention_RETENTION_UNSPECIFIED)
				if err != nil {
					b.Fatal(err)
				}
				if len(hits) != 3 {
					b.Fatalf("want 3 hits, got %d", len(hits))
				}
			}
			// Per-record cost is the number that generalises; ns/op alone
			// is only meaningful next to its N.
			b.ReportMetric(float64(n), "records")
		})
	}
}

// BenchmarkVectorSearchDims varies embedding width at fixed corpus size.
// If cost tracks D closely the arithmetic dominates; if it barely moves,
// the fixed per-record overhead (decrypt + unmarshal) does — which is the
// difference between "optimise the maths" and "stop touching every record".
func BenchmarkVectorSearchDims(b *testing.B) {
	const n = 2_000
	for _, dim := range []int{384, 768, 1536} {
		b.Run(fmt.Sprintf("D=%d", dim), func(b *testing.B) {
			s := benchStore(b)
			seedVectors(b, s, n, dim)
			q := randVec(rand.New(rand.NewSource(7)), dim)

			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := vectorSearch(s, q, 3, Everyone(), "",
					lobslawv1.Retention_RETENTION_UNSPECIFIED); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkScanDecryptOnly is layer 1 in isolation: walk the bucket and
// decrypt every record, doing nothing else. Whatever this costs,
// vectorSearch can never be faster while it visits every record — no
// amount of arithmetic tuning gets below this floor.
func BenchmarkScanDecryptOnly(b *testing.B) {
	const n = 10_000
	s := benchStore(b)
	seedVectors(b, s, n, benchDim)

	b.ReportAllocs()
	for b.Loop() {
		var bytes int
		if err := s.ForEach(BucketVectorRecords, func(_ string, v []byte) error {
			bytes += len(v)
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkScanDecryptUnmarshal adds layer 2. The delta against
// BenchmarkScanDecryptOnly is what proto decoding costs — most of which is
// VectorRecord.Text, a field scoring never looks at.
func BenchmarkScanDecryptUnmarshal(b *testing.B) {
	const n = 10_000
	s := benchStore(b)
	seedVectors(b, s, n, benchDim)

	b.ReportAllocs()
	for b.Loop() {
		if err := s.ForEach(BucketVectorRecords, func(_ string, raw []byte) error {
			var v lobslawv1.VectorRecord
			return proto.Unmarshal(raw, &v)
		}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCosineOnly is layers 3+4 with no storage involved: the maths
// the algorithm genuinely requires, over already-decoded vectors. Compare
// against BenchmarkVectorSearch at the same N to see what fraction of the
// hot path is actually similarity computation.
func BenchmarkCosineOnly(b *testing.B) {
	const n = 10_000
	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = randVec(rng, benchDim)
	}
	q := randVec(rand.New(rand.NewSource(7)), benchDim)
	qn := norm(q)

	b.ReportAllocs()
	for b.Loop() {
		var best float32
		for _, v := range vecs {
			if score := dot(q, v) / (qn * norm(v)); score > best {
				best = score
			}
		}
		_ = best
	}
}

// BenchmarkNormRecompute isolates one specific waste: norm(v.Embedding)
// is a property of the stored vector, recomputed on every query for every
// record. Storing it on VectorRecord at write time removes exactly this.
func BenchmarkNormRecompute(b *testing.B) {
	const n = 10_000
	rng := rand.New(rand.NewSource(42))
	vecs := make([][]float32, n)
	for i := range vecs {
		vecs[i] = randVec(rng, benchDim)
	}

	for b.Loop() {
		var sum float32
		for _, v := range vecs {
			sum += norm(v)
		}
		_ = sum
	}
}

// BenchmarkSortAllVsTopK shows the cost of sorting every candidate before
// truncating to limit. vectorSearch appends every passing record then
// sort.Slice's the lot — O(N log N) time and O(N) memory for a top-3
// result. A bounded heap is O(N log K) and O(K).
func BenchmarkSortAllVsTopK(b *testing.B) {
	const n = 10_000
	b.Run("sort-all", func(b *testing.B) {
		rng := rand.New(rand.NewSource(1))
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			hits := make([]searchHit, n)
			for j := range hits {
				hits[j] = searchHit{score: rng.Float32()}
			}
			b.StartTimer()
			// Mirrors vectorSearch exactly: sort everything, keep 3.
			sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
			_ = hits[:3]
		}
	})
}

// TestCosineAccumulationPrecision documents a numerical property rather
// than asserting a bug: dot and norm accumulate in float32, so error
// grows with dimension. nullclaw's equivalent accumulates in f64 and
// casts once at the end.
//
// This is not currently wrong — cosine is a ranking signal, and ranking
// tolerates small absolute error. It is worth pinning because if recall
// ever gains a MINIMUM SCORE THRESHOLD, an absolute cutoff starts caring
// about absolute accuracy, and this is where the error comes from.
func TestCosineAccumulationPrecision(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(99))
	v := randVec(rng, 4096) // exaggerated width to make drift visible

	got := float64(norm(v))

	// Reference: identical algorithm, float64 accumulator.
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	want := math.Sqrt(sum)

	relErr := math.Abs(got-want) / want
	t.Logf("float32 norm=%.10f float64 norm=%.10f rel_err=%.3e", got, want, relErr)

	// Loose bound: this test exists to surface the magnitude in test
	// output, not to enforce a tight tolerance. Tighten it only alongside
	// a decision that absolute score values matter.
	if relErr > 1e-3 {
		t.Errorf("float32 accumulation drift %.3e exceeds 1e-3 — revisit "+
			"accumulator width before relying on absolute scores", relErr)
	}
}
