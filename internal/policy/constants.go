package policy

import "time"

// Package policy defaults, resource limits, and protocol bounds.
const (
	policyApplyTimeout time.Duration = 5 * time.Second
	// Match the kernel's bounded symlink traversal; cycles must fail closed.
	maxPathSymlinks int = 40
)
