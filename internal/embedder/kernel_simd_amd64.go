//go:build goexperiment.simd && amd64

package embedder

import "simd/archsimd"

// The AVX2 path, compiled only under GOEXPERIMENT=simd on amd64.
//
// archsimd is explicitly outside Go's compatibility promise and only
// exists for amd64, so this file is an ACCELERATOR, never the
// definition. dotGeneric in kernel.go remains the specification: it
// compiles everywhere, it is what arm64 and any non-experiment build
// runs, and TestSIMDMatchesGeneric asserts this file agrees with it.
//
// Two gates have to pass before it is used. The build tag decides
// whether the code exists; archsimd.X86.AVX2() decides whether this
// CPU can run it — the experiment can be enabled on a machine without
// AVX2, and executing these instructions there would fault rather
// than degrade.

const kernelName = "simd-avx2"

// useSIMD is resolved once at start-up rather than per call.
var useSIMD = archsimd.X86.AVX2()

func dot(a, b []float32) float32 {
	if useSIMD {
		return dotSIMD(a, b)
	}
	return dotGeneric(a, b)
}

// dotSIMD is dotGeneric with the four scalar accumulators replaced by
// four 8-wide vector accumulators — 32 products in flight instead of
// four, with the same independent-chain reasoning behind the count.
//
// FMA via MulAdd: one instruction for the multiply and the add, and
// no intermediate rounding of the product.
//
// The tail is dotGeneric, not a duplicated scalar loop, so the two
// paths cannot drift.
func dotSIMD(a, b []float32) float32 {
	const lanes = 8
	acc0 := archsimd.BroadcastFloat32x8(0)
	acc1 := archsimd.BroadcastFloat32x8(0)
	acc2 := archsimd.BroadcastFloat32x8(0)
	acc3 := archsimd.BroadcastFloat32x8(0)

	i := 0
	for ; i+4*lanes <= len(a); i += 4 * lanes {
		acc0 = archsimd.LoadFloat32x8Slice(a[i:]).MulAdd(archsimd.LoadFloat32x8Slice(b[i:]), acc0)
		acc1 = archsimd.LoadFloat32x8Slice(a[i+lanes:]).MulAdd(archsimd.LoadFloat32x8Slice(b[i+lanes:]), acc1)
		acc2 = archsimd.LoadFloat32x8Slice(a[i+2*lanes:]).MulAdd(archsimd.LoadFloat32x8Slice(b[i+2*lanes:]), acc2)
		acc3 = archsimd.LoadFloat32x8Slice(a[i+3*lanes:]).MulAdd(archsimd.LoadFloat32x8Slice(b[i+3*lanes:]), acc3)
	}
	for ; i+lanes <= len(a); i += lanes {
		acc0 = archsimd.LoadFloat32x8Slice(a[i:]).MulAdd(archsimd.LoadFloat32x8Slice(b[i:]), acc0)
	}

	var buf [lanes]float32
	acc0.Add(acc1).Add(acc2.Add(acc3)).StoreSlice(buf[:])
	sum := ((buf[0] + buf[1]) + (buf[2] + buf[3])) + ((buf[4] + buf[5]) + (buf[6] + buf[7]))
	return sum + dotGeneric(a[i:], b[i:])
}
