package backup

// Package policy defaults, resource limits, and protocol bounds.
const (
	maxGenerationIDBytes int   = 128
	maxManifestBytes     int64 = 16 << 20
)
