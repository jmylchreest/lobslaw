package scheduler

import (
	"context"
	"fmt"
	"sync"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// TaskHandler runs a scheduled task. Returning an error logs but
// doesn't block future firings. Handlers are expected to be idempotent
// — in a partition the same firing MAY dispatch twice (see the
// lobslaw-cluster-claim decision's partition caveat).
type TaskHandler func(ctx context.Context, task *lobslawv1.ScheduledTaskRecord) error

// CommitmentHandler runs a one-shot commitment. Same idempotency
// contract as TaskHandler.
type CommitmentHandler func(ctx context.Context, c *lobslawv1.AgentCommitment) error

// HandlerRegistry maps HandlerRef strings to concrete functions.
// Populated at boot by whoever wires the scheduler; mutable afterward
// so tests can swap handlers between iterations.
type HandlerRegistry struct {
	mu          sync.RWMutex
	tasks       map[string]TaskHandler
	commitments map[string]CommitmentHandler
}

func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{
		tasks:       make(map[string]TaskHandler),
		commitments: make(map[string]CommitmentHandler),
	}
}

// RegisterTask installs a handler for ref. Overwrites any prior
// entry — the last-write-wins pattern matches the registry's use as
// a boot-time wiring site where the final write is authoritative.
func (r *HandlerRegistry) RegisterTask(ref string, h TaskHandler) error {
	if ref == "" {
		return fmt.Errorf("scheduler.HandlerRegistry: ref required")
	}
	if h == nil {
		return fmt.Errorf("scheduler.HandlerRegistry: handler required for ref %q", ref)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[ref] = h
	return nil
}

// RegisterCommitment installs a commitment handler. See RegisterTask.
func (r *HandlerRegistry) RegisterCommitment(ref string, h CommitmentHandler) error {
	if ref == "" {
		return fmt.Errorf("scheduler.HandlerRegistry: ref required")
	}
	if h == nil {
		return fmt.Errorf("scheduler.HandlerRegistry: handler required for ref %q", ref)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commitments[ref] = h
	return nil
}

func (r *HandlerRegistry) GetTaskHandler(ref string) (TaskHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.tasks[ref]
	return h, ok
}

func (r *HandlerRegistry) GetCommitmentHandler(ref string) (CommitmentHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.commitments[ref]
	return h, ok
}
