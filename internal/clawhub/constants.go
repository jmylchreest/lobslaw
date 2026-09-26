package clawhub

// Package policy defaults, resource limits, and protocol bounds.
const (
	maxMetadataBytes          int64 = 1 << 20
	maxErrorOutputRunes       int   = 256
	shareArchiveOverheadBytes int64 = 256 << 10
	shareMetadataFileCount    int   = 2
)

const (
	gzipMagic               string = "\x1f\x8b"
	zipMagicPrefix          string = "PK"
	zipLocalFileMarker      byte   = 0x03
	zipEmptyArchiveMarker   byte   = 0x05
	zipSpannedArchiveMarker byte   = 0x07
	zipSignatureBytes       int    = 4
)
