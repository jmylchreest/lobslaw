package hooks

import "time"

// Package policy defaults, resource limits, and protocol bounds.
const (
	hookWaitDelay     time.Duration = 500 * time.Millisecond
	HookBlockExitCode int           = 2
)
