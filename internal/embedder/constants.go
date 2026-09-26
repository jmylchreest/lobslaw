package embedder

// Package policy defaults, resource limits, and protocol bounds.
const (
	defaultLayerNormEpsilon      float32 = 1e-12
	defaultUnigramUnknownTokenID int32   = 3
	defaultWordPieceMaxWordRunes int     = 100
	wholeClusterLookupLimitBytes int     = 6
)

const (
	safetensorsHeaderBytes int = 8
	float32Bytes           int = 4
)
