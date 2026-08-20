//go:build goexperiment.simd && amd64

package embedder

import (
	"math"
	"math/rand"
	"testing"
)

// THE TWO PATHS MUST AGREE.
//
// This is the test that makes an architecture-specific kernel
// acceptable at all. dotGeneric is the specification — it is what
// arm64 runs, what a non-experiment build runs, and what the golden
// vectors were validated against. dotSIMD is an accelerator, and an
// accelerator that computes something subtly different is not an
// accelerator, it is a second implementation with its own bugs and no
// tests.
//
// Only compiled under GOEXPERIMENT=simd on amd64, which is exactly
// when there are two paths to disagree.
func TestSIMDMatchesGeneric(t *testing.T) {
	t.Parallel()
	if !useSIMD {
		t.Skip("this CPU has no AVX2, so dot() already routes to dotGeneric")
	}
	rng := rand.New(rand.NewSource(23))
	// Lengths around every seam: below one vector, exact multiples of
	// 8 and 32, and the awkward remainders that exercise the scalar
	// tail after the vector body.
	for _, n := range []int{0, 1, 3, 7, 8, 9, 15, 16, 17, 31, 32, 33, 63, 64, 65, 127, 384, 768, 3072} {
		for trial := range 4 {
			a, b := randFloats(rng, n), randFloats(rng, n)
			g, s := dotGeneric(a, b), dotSIMD(a, b)
			// Tolerance scaled to the magnitude of the sum: the two
			// differ only in the ORDER they accumulate, and float32
			// reassociation error grows with the number of terms.
			tol := 1e-5 * (1 + math.Abs(float64(g)))
			if math.Abs(float64(g-s)) > tol {
				t.Errorf("n=%d trial=%d: generic=%v simd=%v (diff %.3e, tol %.3e)",
					n, trial, g, s, math.Abs(float64(g-s)), tol)
			}
		}
	}
}

// Zero and one-element inputs go entirely through the tail, which is
// dotGeneric, so these must be exactly equal rather than close.
func TestSIMDTailIsExactForShortInputs(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(29))
	for _, n := range []int{0, 1, 2, 3, 4, 5, 6, 7} {
		a, b := randFloats(rng, n), randFloats(rng, n)
		if g, s := dotGeneric(a, b), dotSIMD(a, b); g != s {
			t.Errorf("n=%d: generic=%v simd=%v — short inputs should not reach the vector body", n, g, s)
		}
	}
}

func TestTheSIMDBuildReportsItself(t *testing.T) {
	t.Parallel()
	if Kernel() != "simd-avx2" {
		t.Errorf("built with the simd experiment but Kernel() = %q", Kernel())
	}
}
