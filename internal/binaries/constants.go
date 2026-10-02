package binaries

// Package policy defaults, resource limits, and protocol bounds.
const (
	maxErrorOutputRunes     int   = 512
	maxLogOutputRunes       int   = 256
	maxReleaseMetadataBytes int64 = 1 << 20
)

const (
	gzipMagic               string = "\x1f\x8b"
	zipMagicPrefix          string = "PK"
	zipLocalFileMarker      byte   = 0x03
	zipEmptyArchiveMarker   byte   = 0x05
	zipSpannedArchiveMarker byte   = 0x07
	zipSignatureBytes       int    = 4
)

const (
	tarMagicOffset int    = 257
	tarMagic       string = "ustar"
	// Keep the existing detector's requirement for the following terminator byte.
	tarSignatureProbeBytes int = tarMagicOffset + len(tarMagic) + 1
)
