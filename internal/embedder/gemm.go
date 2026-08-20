package embedder

import (
	"runtime"
	"sync"
)

// matmulPacked computes c[m,n] = a[m,k] * W^T for a pre-packed W.
//
// Parallel over row blocks, exactly as matmulBT is, and for the same
// reason: rows are independent, each worker writes a disjoint span of
// c, and the summation order inside any one output element is fixed by
// the k loop rather than by how the work was divided. The result is
// therefore identical however many workers run — asserted, not assumed.
func matmulPacked(a []float32, w *packedWeight, c []float32, m int) {
	if m == 0 || w.n == 0 {
		return
	}
	workers := runtime.GOMAXPROCS(0)
	blocks := (m + mr - 1) / mr
	if workers > blocks {
		workers = blocks
	}
	if workers <= 1 {
		gemmRange(a, w, c, 0, m)
		return
	}
	// Split on mr boundaries so no worker gets a partial tile that
	// another worker also touches.
	blocksPer := (blocks + workers - 1) / workers
	var wg sync.WaitGroup
	for b := 0; b < blocks; b += blocksPer {
		lo := b * mr
		hi := min((b+blocksPer)*mr, m)
		if lo >= m {
			break
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			gemmRange(a, w, c, lo, hi)
		}(lo, hi)
	}
	wg.Wait()
}

// gemmRange fills output rows [lo, hi).
func gemmRange(a []float32, w *packedWeight, c []float32, lo, hi int) {
	k, n := w.k, w.n
	i := lo
	for ; i+mr <= hi; i += mr {
		for p := range w.panels {
			gemmTile(a, w.data[p*k*nr:], c, i, p, k, n)
		}
	}
	// Tail rows: fewer than mr left. Handled by the same tile kernel
	// with a narrower row count rather than a separate code path, so
	// there is only one place the arithmetic can be wrong.
	for ; i < hi; i++ {
		for p := range w.panels {
			gemmTailRow(a, w.data[p*k*nr:], c, i, p, k, n)
		}
	}
}

// gemmTailRow is gemmTile for a single row.
func gemmTailRow(a, wp, c []float32, i, p, k, n int) {
	var acc [nr]float32
	ar := a[i*k : (i+1)*k]
	for x := range k {
		av := ar[x]
		row := wp[x*nr : x*nr+nr]
		for jj := range nr {
			acc[jj] += av * row[jj]
		}
	}
	base := i*n + p*nr
	for jj := range nr {
		if p*nr+jj >= n {
			break
		}
		c[base+jj] = acc[jj]
	}
}
