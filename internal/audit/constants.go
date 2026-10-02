package audit

import "time"

// Package policy defaults, resource limits, and protocol bounds.
const (
	initialScanBufferBytes int           = 64 << 10
	maxAuditEntryBytes     int           = 1 << 20
	DefaultApplyTimeout    time.Duration = 5 * time.Second
)
