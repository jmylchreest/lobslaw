package memory

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
)

var errControlChanged = errors.New("leadership, membership or upgrade state changed; retry with fresh status")

// leadershipFence relies on the pinned Raft implementation calling observer
// filters synchronously on every state change, before its main loop resumes.
// A cancellable lock spans validation AND the unbuffered command enqueue, never the
// future wait. A racing state change must finish the filter before it can accept
// commands in a later term. If it races a send, the bounded enqueue times out.
// BatchApplyCh must remain false: a buffered send would defeat this fence.
type leadershipFence struct {
	gate       chan struct{}
	generation uint64
}

func (n *RaftNode) installControlFence() {
	n.leadership.gate = make(chan struct{}, 1)
	n.Raft.RegisterObserver(raft.NewObserver(nil, false, func(o *raft.Observation) bool {
		if _, ok := o.Data.(raft.RaftState); ok {
			n.leadership.gate <- struct{}{}
			n.leadership.generation++
			<-n.leadership.gate
		}
		return false
	}))
}

func (n *RaftNode) lockLeadership(ctx context.Context) error {
	select {
	case n.leadership.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-n.leadership.gate
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (n *RaftNode) controlGeneration(ctx context.Context) (uint64, error) {
	if err := n.lockLeadership(ctx); err != nil {
		return 0, err
	}
	defer func() { <-n.leadership.gate }()
	if n.Raft.State() != raft.Leader {
		return 0, raft.ErrNotLeader
	}
	return n.leadership.generation, nil
}

func (n *RaftNode) enqueueControl(ctx context.Context, generation uint64, submit func(time.Duration) raft.Future) (raft.Future, error) {
	if err := n.lockLeadership(ctx); err != nil {
		return nil, err
	}
	defer func() { <-n.leadership.gate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if n.leadership.generation != generation || n.Raft.State() != raft.Leader {
		return nil, errControlChanged
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("control operation requires a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, context.DeadlineExceeded
	}
	return submit(remaining), nil
}

type controlLease struct {
	n        *RaftNode
	deferred bool
	once     sync.Once
}

func (n *RaftNode) acquireControl(ctx context.Context) (*controlLease, error) {
	select {
	case n.control <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-n.control
			return nil, err
		}
		return &controlLease{n: n}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.stopWatch:
		return nil, raft.ErrRaftShutdown
	}
}

func (l *controlLease) unlock() { l.once.Do(func() { <-l.n.control }) }
func (l *controlLease) release() {
	if !l.deferred {
		l.unlock()
	}
}

// Raft's timeout bounds enqueue, not commitment. After cancellation, retain the
// lease until the submitted future resolves or Raft shuts down. Later callers
// can cancel while waiting, and at most one unresolved future exists per node.
// In particular a timed-out membership operation cannot overtake preparation.
func (l *controlLease) wait(ctx context.Context, future raft.Future) error {
	done := make(chan error, 1)
	go func() { done <- future.Error() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		l.deferred = true
		go func() { <-done; l.unlock() }()
		return ctx.Err()
	}
}

type controlView struct {
	generation uint64
	state      dataformat.ContractState
	servers    []raft.Server
	index      uint64
}

// readControl returns a lease only on success; callers must release it. Probes
// use the returned immutable view after releasing the lease, then compare a new
// barrier-backed view before submission.
func (n *RaftNode) readControl(ctx context.Context) (*controlLease, controlView, error) {
	var view controlView
	lease, err := n.acquireControl(ctx)
	if err != nil {
		return nil, view, err
	}
	ok := false
	defer func() {
		if !ok {
			lease.release()
		}
	}()
	view.generation, err = n.controlGeneration(ctx)
	if err != nil {
		return nil, view, err
	}
	future, err := n.enqueueControl(ctx, view.generation, func(timeout time.Duration) raft.Future { return n.Raft.Barrier(timeout) })
	if err != nil {
		return nil, view, err
	}
	if err := lease.wait(ctx, future); err != nil {
		return nil, view, err
	}
	view.state, err = n.fsm.store.ContractState()
	if err != nil {
		return nil, view, err
	}
	cfg := n.Raft.GetConfiguration()
	if err := cfg.Error(); err != nil {
		return nil, view, err
	}
	view.servers = cfg.Configuration().Servers
	view.index = n.Raft.AppliedIndex()
	if generation, err := n.controlGeneration(ctx); err != nil || generation != view.generation {
		return nil, view, errControlChanged
	}
	ok = true
	return lease, view, nil
}

func (v controlView) matches(other controlView) bool {
	return v.generation == other.generation && reflect.DeepEqual(v.state, other.state) && configurationFingerprint(v.servers) == configurationFingerprint(other.servers)
}

func controlContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = upgradeTimeout
	}
	return context.WithTimeout(ctx, timeout)
}
