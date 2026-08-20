//go:build goexperiment.simd && amd64

package embedder

import "simd/archsimd"

// gemmTile computes an mr x nr tile of c with the accumulators held in
// vector registers for the whole k loop.
//
// nr is 16, so each output row is two 8-wide vectors: eight
// accumulators for four rows. The k loop broadcasts one element of a
// and issues eight FMAs against two loaded b vectors — no horizontal
// reduction anywhere, which is the entire point of the packed layout.
//
// Falls back to the portable tile when the CPU has no AVX2. The build
// tag decides whether these instructions EXIST; useSIMD decides whether
// this processor can run them, and the experiment can be enabled on one
// that cannot.
func gemmTile(a, wp, c []float32, i, p, k, n int) {
	if !useSIMD {
		gemmTileGeneric(a, wp, c, i, p, k, n)
		return
	}
	a0 := archsimd.BroadcastFloat32x8(0)
	a1, a2, a3 := a0, a0, a0
	b0, b1, b2, b3 := a0, a0, a0, a0

	for x := range k {
		lo := archsimd.LoadFloat32x8Slice(wp[x*nr:])
		hi := archsimd.LoadFloat32x8Slice(wp[x*nr+8:])

		v0 := archsimd.BroadcastFloat32x8(a[i*k+x])
		v1 := archsimd.BroadcastFloat32x8(a[(i+1)*k+x])
		v2 := archsimd.BroadcastFloat32x8(a[(i+2)*k+x])
		v3 := archsimd.BroadcastFloat32x8(a[(i+3)*k+x])

		a0 = v0.MulAdd(lo, a0)
		b0 = v0.MulAdd(hi, b0)
		a1 = v1.MulAdd(lo, a1)
		b1 = v1.MulAdd(hi, b1)
		a2 = v2.MulAdd(lo, a2)
		b2 = v2.MulAdd(hi, b2)
		a3 = v3.MulAdd(lo, a3)
		b3 = v3.MulAdd(hi, b3)
	}

	var buf [nr]float32
	store := func(r int, lo, hi archsimd.Float32x8) {
		lo.StoreSlice(buf[:8])
		hi.StoreSlice(buf[8:])
		base := (i+r)*n + p*nr
		for jj := range nr {
			if p*nr+jj >= n {
				break
			}
			c[base+jj] = buf[jj]
		}
	}
	store(0, a0, b0)
	store(1, a1, b1)
	store(2, a2, b2)
	store(3, a3, b3)
}
