package memory

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// TestSweep measures one query at a range of corpus sizes and reports
// latency, allocation churn, and — the number that decides whether a
// deployment survives rather than merely runs slowly — bytes still
// reachable once the query returns.
//
// Opt-in, because seeding a large corpus takes minutes and 1M records
// writes a 7.7 GB store:
//
//	SWEEP=1 go test ./internal/memory -run TestSweep -v -timeout 2h
//	SWEEP=1 SWEEP_MAX=10000 go test ...   # cap the largest size
//
// Complements the benchmarks in search_bench_test.go rather than
// duplicating them. Those answer "did this change help", with enough
// samples for benchstat to say so; this answers "what does N=1e6 cost",
// which is too slow to benchmark properly.
//
// Read the columns accordingly. churn, retained and db are deterministic
// and exact. LATENCY IS ONE COLD-CACHE SAMPLE and should not be quoted as
// a measurement — use the benchmarks for that. retained is a lower bound:
// it is HeapAlloc after a GC with the result still live, so it captures
// what the returned value pins, not the peak during the query.
func TestSweep(t *testing.T) {
	if os.Getenv("SWEEP") == "" {
		t.Skip("set SWEEP=1 to run")
	}
	max := 100_000
	if v := os.Getenv("SWEEP_MAX"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		max = n
	}

	const dim = 1536
	sizes := []int{1, 10, 100, 1_000, 10_000, 100_000, 1_000_000}
	rng := rand.New(rand.NewSource(7))
	query := make([]float32, dim)
	for i := range query {
		query[i] = rng.Float32()*2 - 1
	}

	text := make([]byte, 600)
	for i := range text {
		text[i] = byte('a' + i%26)
	}

	fmt.Printf("\n%10s  %12s  %12s  %12s  %10s\n",
		"N", "latency", "churn", "retained", "db")
	for _, n := range sizes {
		if n > max {
			continue
		}
		func() {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			key, err := crypto.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			s, err := OpenStore(path, key)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()

			seed := rand.New(rand.NewSource(42))
			emb := make([]float32, dim)
			for i := range n {
				for j := range emb {
					emb[j] = seed.Float32()*2 - 1
				}
				raw, err := proto.Marshal(&lobslawv1.VectorRecord{
					Id:        fmt.Sprintf("vec-%09d", i),
					Embedding: emb,
					Text:      string(text),
					Scope:     "episodic",
					Retention: lobslawv1.Retention_RETENTION_LONG_TERM,
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Put(BucketVectorRecords, fmt.Sprintf("vec-%09d", i), raw); err != nil {
					t.Fatal(err)
				}
			}

			var before, afterChurn, retained runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)

			start := time.Now()
			hits, err := vectorSearch(s, query, 3, Everyone(), "",
				lobslawv1.Retention_RETENTION_UNSPECIFIED)
			elapsed := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}

			runtime.ReadMemStats(&afterChurn)
			// GC with hits still live: whatever survives is what the
			// returned result keeps reachable.
			runtime.GC()
			runtime.ReadMemStats(&retained)

			var dbSize int64
			if fi, err := os.Stat(path); err == nil {
				dbSize = fi.Size()
			}

			fmt.Printf("%10d  %12s  %12s  %12s  %10s   (%d hits)\n",
				n,
				elapsed.Round(time.Microsecond),
				human(afterChurn.TotalAlloc-before.TotalAlloc),
				humanSigned(int64(retained.HeapAlloc)-int64(before.HeapAlloc)),
				human(uint64(dbSize)),
				len(hits))

			runtime.KeepAlive(hits)
		}()
	}
}

func humanSigned(b int64) string {
	if b < 0 {
		return "~0"
	}
	return human(uint64(b))
}

func human(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
