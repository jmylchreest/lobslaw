package storage

import "time"

// Package policy defaults, resource limits, and protocol bounds.
const (
	watchEventBuffer          int           = 128
	DefaultRemotePollInterval time.Duration = 5 * time.Minute
	DefaultApplyTimeout       time.Duration = 5 * time.Second
)
