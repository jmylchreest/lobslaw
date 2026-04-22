package types

import "time"

type ParameterMap map[string]any

type RetryPolicy struct {
	MaxAttempts int    `json:"max_attempts"`
	Backoff     string `json:"backoff"`
	InitialSecs int    `json:"initial_secs"`
}

// ScheduledTaskRecord is a recurring operator-defined cron job.
// ClaimedBy+ClaimExpiresAt implement the Raft-CAS claim that keeps
// a firing singleton across N compute nodes.
type ScheduledTaskRecord struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	Schedule       string       `json:"schedule"`
	HandlerRef     string       `json:"handler_ref"`
	Params         ParameterMap `json:"params,omitempty"`
	RetryPolicy    RetryPolicy  `json:"retry_policy"`
	Enabled        bool         `json:"enabled"`
	CreatedBy      string       `json:"created_by"`
	CreatedAt      time.Time    `json:"created_at"`
	LastRun        time.Time    `json:"last_run"`
	NextRun        time.Time    `json:"next_run"`
	ClaimedBy      NodeID       `json:"claimed_by,omitempty"`
	ClaimExpiresAt time.Time    `json:"claim_expires_at,omitempty"`
}

type CommitmentStatus string

const (
	CommitmentPending   CommitmentStatus = "pending"
	CommitmentDone      CommitmentStatus = "done"
	CommitmentCancelled CommitmentStatus = "cancelled"
)

// AgentCommitment is a one-shot user-originated deferred task.
// Distinct from ScheduledTaskRecord: commitments are one-shot
// ("check back in 2h"), tasks are recurring.
type AgentCommitment struct {
	ID              string           `json:"id"`
	DueAt           time.Time        `json:"due_at"`
	Trigger         string           `json:"trigger"`
	Reason          string           `json:"reason"`
	CreatedFromTurn string           `json:"created_from_turn,omitempty"`
	CreatedFor      string           `json:"created_for"`
	Status          CommitmentStatus `json:"status"`
	HandlerRef      string           `json:"handler_ref,omitempty"`
	Params          ParameterMap     `json:"params,omitempty"`
	ClaimedBy       NodeID           `json:"claimed_by,omitempty"`
	ClaimExpiresAt  time.Time        `json:"claim_expires_at,omitempty"`
}

// Plan is the windowed aggregate returned by PlanService.GetPlan.
type Plan struct {
	Window           time.Duration         `json:"window"`
	Commitments      []AgentCommitment     `json:"commitments,omitempty"`
	ScheduledTasks   []ScheduledTaskRecord `json:"scheduled_tasks,omitempty"`
	InFlight         []InFlightWork        `json:"in_flight,omitempty"`
	CheckBackThreads []CheckBack           `json:"check_back_threads,omitempty"`
}

type InFlightWork struct {
	ID           string    `json:"id"`
	Goal         string    `json:"goal"`
	LastProgress time.Time `json:"last_progress"`
	Status       string    `json:"status"`
	Blockers     []string  `json:"blockers,omitempty"`
}

type CheckBack struct {
	ID              string    `json:"id"`
	OriginalRequest string    `json:"original_request"`
	ScheduledFor    time.Time `json:"scheduled_for"`
}
