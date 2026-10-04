package main

import "time"

// Command defaults stay here; server-side contracts belong to their packages.
const (
	defaultRPCTimeout             time.Duration = 10 * time.Second
	defaultRestoreTimeout         time.Duration = 30 * time.Minute
	defaultConsolidationListLimit int           = 50
	defaultTraceListLimit         int           = 20
	defaultAuditListLimit         int           = 50
	enrolRPCTimeout               time.Duration = 15 * time.Second
	enrolPollInterval             time.Duration = 2 * time.Second
	doctorProviderProbeTimeout    time.Duration = 4 * time.Second
	defaultEmbedEvalLimit         int           = 200
	embedEvalTimeout              time.Duration = 30 * time.Minute
	// A bounded day count avoids duration overflow and catches likely input mistakes.
	maxBackupRetentionDays int64 = 100000
)
