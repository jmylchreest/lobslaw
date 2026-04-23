// Package plan implements PlanService: the "what's on your plate?"
// aggregation surface. GetPlan walks the scheduled_tasks and
// commitments buckets and returns due-in-window entries. AddCommitment
// and CancelCommitment are the write path for one-shot user
// commitments (recurring scheduled tasks come from config, not an RPC).
package plan
