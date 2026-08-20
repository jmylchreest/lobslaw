package embedder

// weight is a projection matrix in whatever form THIS BUILD's kernel
// runs fastest against.
//
// The two kernels genuinely want different layouts, and that is a
// measured result rather than a preference. On the same machine, same
// model, 64 tokens:
//
//	                     dot kernel   packed GEMM
//	portable                 524 ms       614 ms   (best of 8 tile shapes)
//	SIMD (AVX2)              285 ms       184 ms
//
// With vectors, holding a 4x16 tile of accumulators in registers
// across the k loop removes the per-element horizontal reduction and
// wins by 1.6x. Without them, sixty-four accumulators spill to stack
// and every FMA becomes two memory operations — every tile shape from
// 4x4 to 4x32 lost to the dot kernel it replaced.
//
// So the layout is chosen per build. The alternative — one layout
// everywhere — would mean deliberately shipping the slower kernel to
// one of the two, and on the portable build that is the one every
// arm64 node runs.
//
// Both are held to the SAME golden vectors, which is what makes two
// implementations acceptable rather than two sets of bugs.
type weight struct {
	bt     []float32 // [n][k] as the checkpoint ships it
	k, n   int
	packed *packedWeight // nil on builds whose kernel reads bt directly
}

// newWeight takes ownership of bt.
func newWeight(bt []float32, k, n int) *weight {
	w := &weight{bt: bt, k: k, n: n}
	prepareWeight(w)
	return w
}

// apply computes c[m,n] = a[m,k] * w^T.
func (w *weight) apply(a, c []float32, m int) { applyWeight(w, a, c, m) }
