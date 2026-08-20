//go:build !(goexperiment.simd && amd64)

package embedder

// The portable path: every architecture, and amd64 without
// GOEXPERIMENT=simd.
//
// This is the DEFINITION of what the encoder computes. The AVX2 files
// are an accelerator that must agree with it, not an alternative to
// it — which is why the golden fixtures are the same for both and why
// neither is allowed its own expected values.
//
// The packed-GEMM layout does not exist on this build at all: it
// measured SLOWER here (see weight.go), so shipping it would mean
// carrying dead code and a second copy of every weight for nothing.

const kernelName = "generic"

func dot(a, b []float32) float32 { return dotGeneric(a, b) }
