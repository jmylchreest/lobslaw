// Package scheduler implements the scheduled-task and commitment
// loops. Both use Raft-CAS claim semantics so a task runs on exactly
// one node in a cluster. Hosts the PlanService that aggregates
// upcoming commitments, scheduled tasks, and in-flight work for the
// "what's your plan today?" surface.
package scheduler
