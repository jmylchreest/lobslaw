package embedder

import (
	"math"
	"math/rand"
	"testing"
)

// naiveMatmulBT is the definition, written for obviousness rather than
// speed, so the optimised kernel has something independent to be
// checked against. If both were written for speed they would share
// their bugs.
func naiveMatmulBT(a, bt []float32, m, k, n int) []float32 {
	c := make([]float32, m*n)
	for i := range m {
		for j := range n {
			var s float32
			for x := range k {
				s += a[i*k+x] * bt[j*k+x]
			}
			c[i*n+j] = s
		}
	}
	return c
}

func randFloats(rng *rand.Rand, n int) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = rng.Float32()*2 - 1
	}
	return out
}

// Shapes chosen to exercise the kernel's seams rather than to look
// realistic: k values that are not multiples of 4 or 8 hit the scalar
// tail, m=1 hits the serial path, and large m crosses the parallel
// threshold so the block split is covered too.
var kernelShapes = []struct{ m, k, n int }{
	{1, 1, 1}, {1, 3, 1}, {1, 7, 5}, {2, 8, 3}, {3, 9, 4},
	{4, 31, 7}, {8, 32, 8}, {5, 33, 6}, {17, 64, 13},
	{64, 128, 64}, {128, 384, 96},
}

func TestMatmulMatchesTheNaiveDefinition(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	for _, s := range kernelShapes {
		a := randFloats(rng, s.m*s.k)
		bt := randFloats(rng, s.n*s.k)
		got := make([]float32, s.m*s.n)
		matmulBT(a, bt, got, s.m, s.k, s.n)
		want := naiveMatmulBT(a, bt, s.m, s.k, s.n)
		for i := range want {
			// Tolerance, not equality: the kernel reassociates the
			// sum across four accumulators (and eight lanes under
			// SIMD), so it lands a few ULP from a strict left-to-right
			// reduction. That is expected; a systematic error is not.
			if d := math.Abs(float64(got[i] - want[i])); d > 1e-4 {
				t.Fatalf("shape %dx%dx%d element %d: got %v want %v (diff %.2e)",
					s.m, s.k, s.n, i, got[i], want[i], d)
			}
		}
	}
}

// DETERMINISM. The parallel split must not change the result.
//
// Rows are independent and each worker writes a disjoint span, so the
// summation order within any one dot product is identical however the
// work is divided. If that ever stopped being true, embeddings would
// differ run to run by an amount too small to notice and too large to
// explain — so this asserts bit-exact equality, not tolerance.
func TestTheParallelSplitIsBitExact(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(11))
	const m, k, n = 96, 384, 96
	a := randFloats(rng, m*k)
	bt := randFloats(rng, n*k)

	first := make([]float32, m*n)
	matmulBT(a, bt, first, m, k, n)
	for range 8 {
		again := make([]float32, m*n)
		matmulBT(a, bt, again, m, k, n)
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("element %d differs between runs: %v vs %v", i, first[i], again[i])
			}
		}
	}
}

// A zero-sized output must be a no-op rather than a panic: the empty
// input fixture reaches the forward pass with no tokens at all.
func TestMatmulHandlesEmptyShapes(t *testing.T) {
	t.Parallel()
	matmulBT(nil, nil, nil, 0, 4, 4)
	matmulBT(nil, nil, nil, 4, 4, 0)
}

func TestDotGenericMatchesASimpleSum(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(3))
	for _, n := range []int{0, 1, 2, 3, 4, 5, 7, 8, 15, 16, 31, 32, 33, 768} {
		a, b := randFloats(rng, n), randFloats(rng, n)
		var want float32
		for i := range a {
			want += a[i] * b[i]
		}
		if got := dotGeneric(a, b); math.Abs(float64(got-want)) > 1e-4 {
			t.Errorf("n=%d: dotGeneric = %v, simple sum = %v", n, got, want)
		}
	}
}

// The build must report which kernel it got, or an operator has no way
// to tell whether a SIMD build is actually taking the SIMD path.
func TestKernelIsNamed(t *testing.T) {
	t.Parallel()
	switch Kernel() {
	case "generic", "simd-avx2":
	default:
		t.Errorf("unexpected kernel name %q", Kernel())
	}
}
