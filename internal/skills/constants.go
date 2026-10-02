package skills

import "time"

// Package policy defaults, resource limits, and protocol bounds.
const (
	DefaultInvocationTimeout time.Duration = 30 * time.Second
	maxInvocationStdoutBytes int           = 1 << 20
	maxInvocationStderrBytes int           = 64 << 10
)
