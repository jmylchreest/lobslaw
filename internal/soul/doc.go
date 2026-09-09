// Package soul loads the operator's SOUL.md baseline and merges a persisted,
// bounded tuning overlay. File reload, tuning, and rollback share one effective
// snapshot; the file itself is never written by agent tools.
package soul
