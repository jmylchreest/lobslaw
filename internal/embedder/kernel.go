package embedder

import (
	"runtime"
	"sync"
)

// The one hot loop. Everything else in a transformer encoder is
// cheap by comparison: at 12 layers x 768 hidden these matmuls are
// the overwhelming majority of the work, so this is the only place
// worth optimising and the only place worth an architecture-specific
// path.
//
// LAYOUT. b is stored TRANSPOSED — [n, k] rather than [k, n] — so
// every dot product reads two contiguous runs. That is not a
// micro-optimisation; the transposed layout is what HuggingFace
// checkpoints already store (nn.Linear weights are [out, in]), so
// the alternative would be transposing 100M+ parameters at load
// time to then read them with a stride.

// dotGeneric is the portable inner product.
//
// EIGHT accumulators rather than one, because a single accumulator
// serialises on the FPU's add latency: each iteration waits for the
// previous add to retire. Independent chains let the pipeline stay
// full, and this is the only lever the portable path has — it has no
// vector types, and every blocked-tile shape from 4x4 to 4x32 measured
// SLOWER than this loop (see weight_nosimd.go).
//
// Eight rather than four is worth ~7% on a real encode, measured; the
// count is bounded by how many partial sums the compiler will keep in
// registers, and past that it spills and gives the gain back.
//
// The partial sums are added in a FIXED order at the end, so this is
// deterministic: same inputs, same result, on every machine.
func dotGeneric(a, b []float32) float32 {
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	i := 0
	for ; i+8 <= len(a); i += 8 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
		s4 += a[i+4] * b[i+4]
		s5 += a[i+5] * b[i+5]
		s6 += a[i+6] * b[i+6]
		s7 += a[i+7] * b[i+7]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return ((s0 + s1) + (s2 + s3)) + ((s4 + s5) + (s6 + s7))
}

// matmulBT computes c[m,n] = a[m,k] * b[n,k]^T.
//
// Parallel over ROWS of the output. Rows are independent and each
// writes a disjoint span of c, so there is no sharing and no
// synchronisation beyond the join.
//
// Deliberately NOT parallel over the inner dot: that would need a
// reduction across goroutines, whose summation order would depend on
// scheduling, which would make the result non-deterministic run to
// run. A recall system that returns marginally different vectors on
// different days is a debugging nightmare nobody would ever suspect.
func matmulBT(a, bt, c []float32, m, k, n int) {
	if m*n == 0 {
		return
	}
	// Below this the goroutine setup costs more than the work saved.
	// Measured on the projection shapes a short sequence produces,
	// where m is small enough that a serial pass wins outright.
	const parallelThreshold = 8192
	workers := min(runtime.GOMAXPROCS(0), m)
	if workers <= 1 || m*n*k < parallelThreshold {
		matmulRange(a, bt, c, k, n, 0, m)
		return
	}

	var wg sync.WaitGroup
	// Contiguous BLOCKS rather than a strided i += workers split:
	// each worker then walks a contiguous span of both a and c, which
	// is what the prefetcher expects.
	block := (m + workers - 1) / workers
	for lo := 0; lo < m; lo += block {
		hi := min(lo+block, m)
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()
			matmulRange(a, bt, c, k, n, lo, hi)
		}(lo, hi)
	}
	wg.Wait()
}

// matmulRange fills output rows [lo, hi).
func matmulRange(a, bt, c []float32, k, n, lo, hi int) {
	for i := lo; i < hi; i++ {
		ar := a[i*k : (i+1)*k]
		row := c[i*n : (i+1)*n]
		for j := range n {
			row[j] = dot(ar, bt[j*k:(j+1)*k])
		}
	}
}

// addBias adds a per-column bias to every row of x[m,n].
func addBias(x, bias []float32, m, n int) {
	if len(bias) == 0 {
		return
	}
	for i := range m {
		row := x[i*n : (i+1)*n]
		for j := range n {
			row[j] += bias[j]
		}
	}
}
