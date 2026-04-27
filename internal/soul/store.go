package soul

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TuneState is the bounded mutable subset of SoulConfig the agent
// may modify at runtime. Pointer-typed fields encode "explicitly
// set" vs "inherit from baseline" — when the operator edits SOUL.md
// to lower sarcasm, untuned fields follow the new baseline while
// fields the agent has explicitly tuned stay pinned.
//
// The shape mirrors the SoulTuneState proto wire format. Mapping
// happens in the adapter wired by the node package; the soul
// package itself has no proto / raft dependency.
type TuneState struct {
	Name       *string
	Excitement *int
	Formality  *int
	Directness *int
	Sarcasm    *int
	Humor      *int
	EmojiUsage *string
	// Fragments distinguishes "no overlay" (nil) from "explicitly
	// empty" (non-nil, len=0). The latter is the user clearing
	// remembered facts.
	Fragments *[]string
	UpdatedAt time.Time
	UpdatedBy string
}

// Clone returns a deep copy. Mutators read-modify-write a clone and
// hand the result to the store; the in-memory cache only updates
// after Put returns successfully.
func (t *TuneState) Clone() *TuneState {
	if t == nil {
		return &TuneState{}
	}
	out := *t
	if t.Name != nil {
		v := *t.Name
		out.Name = &v
	}
	if t.Excitement != nil {
		v := *t.Excitement
		out.Excitement = &v
	}
	if t.Formality != nil {
		v := *t.Formality
		out.Formality = &v
	}
	if t.Directness != nil {
		v := *t.Directness
		out.Directness = &v
	}
	if t.Sarcasm != nil {
		v := *t.Sarcasm
		out.Sarcasm = &v
	}
	if t.Humor != nil {
		v := *t.Humor
		out.Humor = &v
	}
	if t.EmojiUsage != nil {
		v := *t.EmojiUsage
		out.EmojiUsage = &v
	}
	if t.Fragments != nil {
		frags := append([]string(nil), (*t.Fragments)...)
		out.Fragments = &frags
	}
	return &out
}

// TuneStore is the persistence surface the Adjuster writes through.
// Production wires a raft-backed implementation; tests use
// MemoryTuneStore for fast in-process state without a Raft cluster.
//
// Get returns (nil, nil) when no overlay record exists yet.
type TuneStore interface {
	Get(ctx context.Context) (*TuneState, error)
	Put(ctx context.Context, state *TuneState) error
	Rollback(ctx context.Context, steps int) (*TuneState, error)
}

// MemoryTuneStore is the in-process implementation used by tests +
// single-node-no-raft setups. Keeps a ring of past versions so
// HistoryRollback works without external state.
type MemoryTuneStore struct {
	mu      sync.Mutex
	current *TuneState
	history []*TuneState
}

// MaxMemoryTuneHistory matches the Raft service's history depth so
// the test path and prod path behave identically for HistoryRollback
// edge cases.
const MaxMemoryTuneHistory = 20

// NewMemoryTuneStore returns an empty store.
func NewMemoryTuneStore() *MemoryTuneStore {
	return &MemoryTuneStore{}
}

// Get returns a clone of the current tune. Cloning means callers
// can mutate freely without disturbing the store's copy.
func (m *MemoryTuneStore) Get(_ context.Context) (*TuneState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current == nil {
		return nil, nil
	}
	return m.current.Clone(), nil
}

// Put records the new state, pushing the previous current to history
// (capped at MaxMemoryTuneHistory).
func (m *MemoryTuneStore) Put(_ context.Context, state *TuneState) error {
	if state == nil {
		return errors.New("soul tune: state required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != nil {
		m.history = append(m.history, m.current)
		if len(m.history) > MaxMemoryTuneHistory {
			m.history = m.history[len(m.history)-MaxMemoryTuneHistory:]
		}
	}
	m.current = state.Clone()
	return nil
}

// Rollback promotes a past version to current. steps=1 is the most
// recent prior version. Promoted state's UpdatedAt is left as-is so
// rollback is distinguishable from a fresh edit in audit logs.
func (m *MemoryTuneStore) Rollback(ctx context.Context, steps int) (*TuneState, error) {
	if steps < 1 {
		return nil, errors.New("soul tune: steps must be >= 1")
	}
	m.mu.Lock()
	if len(m.history) < steps {
		depth := len(m.history)
		m.mu.Unlock()
		return nil, fmt.Errorf("soul tune: only %d history entries; cannot rollback %d steps", depth, steps)
	}
	idx := len(m.history) - steps
	picked := m.history[idx].Clone()
	m.mu.Unlock()
	if err := m.Put(ctx, picked); err != nil {
		return nil, err
	}
	return picked, nil
}
