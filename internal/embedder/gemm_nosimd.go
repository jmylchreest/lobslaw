//go:build !(goexperiment.simd && amd64)

package embedder

// The portable build has one tile kernel, and it is the definition.
func gemmTile(a, wp, c []float32, i, p, k, n int) {
	gemmTileGeneric(a, wp, c, i, p, k, n)
}
