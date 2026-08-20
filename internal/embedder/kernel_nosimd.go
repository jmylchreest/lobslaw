//go:build !(goexperiment.simd && amd64)

package embedder

const (
	kernelName = "generic"

	mr = 4
	nr = 16
)

func dot(a, b []float32) float32 { return dotGeneric(a, b) }
