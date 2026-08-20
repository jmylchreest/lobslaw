package embedder

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// DOES IT SCALE LINEARLY?
//
// The question that decides whether a backfill is viable. Embedding
// one record in 50 ms is fine; embedding 10,000 is only fine if the
// ten-thousandth costs the same as the first. Two things could break
// that and neither would be obvious: heap growth from per-call
// allocations outrunning the collector, and goroutine churn from ~360
// matmuls per encode each spawning workers.
//
// Reports per-record cost at increasing counts. Flat means linear.
//
// Opt-in: this deliberately runs for minutes.
func TestBackfillScaling(t *testing.T) {
	if os.Getenv("LOBSLAW_EMBEDDER_SCALING") == "" {
		t.Skip("set LOBSLAW_EMBEDDER_SCALING=1 to run the scaling measurement")
	}
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()

	// A record of roughly the length a real memory tokenises to.
	ids := make([]int32, 0, 32)
	ids = append(ids, 0)
	for range 30 {
		ids = append(ids, 38937)
	}
	ids = append(ids, 2)

	// Warm: the first call faults in the mapped weights, and charging
	// a gigabyte of page faults to the first batch would make small
	// counts look artificially slow and the trend artificially good.
	for range 20 {
		_ = m.Embed(ids)
	}

	fmt.Printf("\n  %8s %12s %14s %12s %10s\n", "records", "elapsed", "per record", "records/s", "heap MB")
	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)

	for _, n := range []int{100, 400, 1600} {
		start := time.Now()
		for range n {
			_ = m.Embed(ids)
		}
		elapsed := time.Since(start)

		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		heap := float64(ms.HeapAlloc) / (1 << 20)

		per := elapsed / time.Duration(n)
		fmt.Printf("  %8d %12s %14s %12.1f %10.1f\n",
			n, elapsed.Round(time.Millisecond), per.Round(time.Microsecond),
			float64(n)/elapsed.Seconds(), heap)
	}

	// And the batch entry point, which is the one a backfill uses.
	seqs := make([][]int32, 400)
	for i := range seqs {
		seqs[i] = ids
	}
	start := time.Now()
	_ = m.EmbedBatch(seqs)
	fmt.Printf("  EmbedBatch(400): %s (%s per record)\n\n",
		time.Since(start).Round(time.Millisecond),
		(time.Since(start) / 400).Round(time.Microsecond))
}
