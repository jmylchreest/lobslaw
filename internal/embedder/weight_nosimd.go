//go:build !(goexperiment.simd && amd64)

package embedder

// weight is a projection matrix in whatever form THIS BUILD's kernel
// runs fastest against — and the two builds genuinely want different
// forms. That is a measured result, not a preference. Same machine,
// same model, 64 tokens:
//
//	                     dot kernel   packed GEMM
//	portable                 524 ms       614 ms   (best of 8 tile shapes)
//	SIMD (AVX2)              285 ms       184 ms
//
// With vectors, holding a 4x16 tile of accumulators in registers
// across the k loop removes the per-element horizontal reduction and
// wins by 1.6x. Without them, sixty-four accumulators spill to stack
// and every FMA becomes two memory operations — every shape from 4x4
// to 4x32 lost to the kernel it replaced.
//
// So this build keeps the checkpoint's own [n][k] layout and the dot
// kernel. It does not pack at all, which also spares it a second copy
// of every weight.
//
// Both forms are held to the SAME golden vectors. That is what makes
// two kernels acceptable rather than two sets of bugs.
type weight struct {
	bt   []float32 // [n][k], exactly as the checkpoint ships it
	k, n int
}

// newWeight takes ownership of bt.
func newWeight(bt []float32, k, n int) *weight {
	return &weight{bt: bt, k: k, n: n}
}

// apply computes c[m,n] = a[m,k] * w^T.
func (w *weight) apply(a, c []float32, m int) {
	matmulBT(a, w.bt, c, m, w.k, w.n)
}
