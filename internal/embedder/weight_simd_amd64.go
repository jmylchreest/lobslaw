//go:build goexperiment.simd && amd64

package embedder

// prepareWeight packs into the tile layout the vector kernel needs.
//
// The unpacked copy is DROPPED afterwards. Holding both would double
// the resident weights — 1.1 GB becomes 2.2 GB on multilingual-e5-base
// and 4.4 GB on bge-m3 — to keep a layout nothing on this build reads.
func prepareWeight(w *weight) {
	w.packed = packWeight(w.bt, w.k, w.n)
	w.bt = nil
}

func applyWeight(w *weight, a, c []float32, m int) {
	matmulPacked(a, w.packed, c, m)
}
