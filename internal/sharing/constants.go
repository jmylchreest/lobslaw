package sharing

// Package policy defaults, resource limits, and protocol bounds.
const (
	MaxInputs               int = 64
	MaxSourcePromptBytes    int = 16 << 10
	MaxInputBytes           int = 4096
	MaxBoundPromptBytes     int = 32 << 10
	MaxOriginReferenceBytes int = 512
	MaxOriginCatalogBytes   int = 2048
	MaxOriginFormatBytes    int = 32
	MaxIdentifierBytes      int = 128
)
