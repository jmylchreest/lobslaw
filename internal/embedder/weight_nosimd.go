//go:build !(goexperiment.simd && amd64)

package embedder

// prepareWeight does nothing: this build's kernel reads the
// checkpoint's own [n][k] layout directly, so packing would cost load
// time and a second copy of every weight to produce a layout that
// measured SLOWER here.
func prepareWeight(*weight) {}

func applyWeight(w *weight, a, c []float32, m int) {
	matmulBT(a, w.bt, c, m, w.k, w.n)
}
