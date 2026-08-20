//go:build goexperiment.simd && amd64

package embedder

// weight for the vector kernel: packed into the tile layout at load.
// See weight_nosimd.go for why the two builds differ and for the
// measurements behind it.
type weight struct {
	packed *packedWeight
}

// newWeight packs bt and DROPS the unpacked copy.
//
// Holding both would double the resident weights — multilingual-e5-base
// from 1.1 GB to 2.2 GB, bge-m3 from 2.2 GB to 4.4 GB — to keep a
// layout nothing on this build reads.
func newWeight(bt []float32, k, n int) *weight {
	return &weight{packed: packWeight(bt, k, n)}
}

// apply computes c[m,n] = a[m,k] * w^T.
func (w *weight) apply(a, c []float32, m int) {
	matmulPacked(a, w.packed, c, m)
}
