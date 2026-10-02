package scheduler

import "time"

// Package policy defaults, resource limits, and protocol bounds.
const (
	DefaultClaimTTL         time.Duration = 5 * time.Minute
	DefaultMaxSleep         time.Duration = 60 * time.Second
	DefaultWakeDebounce     time.Duration = 50 * time.Millisecond
	DefaultMinFireInterval  time.Duration = 250 * time.Millisecond
	DefaultRaftApplyTimeout time.Duration = 5 * time.Second
	stallSleepMultiplier    int           = 3
	barrierTimeout          time.Duration = 5 * time.Second
)
