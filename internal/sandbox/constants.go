package sandbox

import (
	"io/fs"
	"time"
)

// Package policy defaults, resource limits, and protocol bounds.
const (
	probeTimeout              time.Duration = 5 * time.Second
	defaultRejectWritableMask fs.FileMode   = 0o022
)
