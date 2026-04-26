package singleton

import (
	"sync"
)

// LeaderSource is the slice of behaviour LeaderGate needs from the
// underlying raft node. internal/memory.RaftNode satisfies this
// (IsLeader + a leadership transition channel via Subscribe).
type LeaderSource interface {
	IsLeader() bool
}

// LeaderGate is a Gate that pins ownership of every name to whoever
// holds raft leadership. Multiple singletons share the same answer;
// the name parameter only labels logs today. A future per-name lease
// implementation can replace it without touching callers.
type LeaderGate struct {
	source LeaderSource

	mu          sync.Mutex
	subscribers map[chan bool]struct{}
	last        bool
}

// NewLeaderGate constructs a Gate that emits transitions whenever
// the parent calls Publish (typically from the raft state-watcher).
// Initial state is taken from source.IsLeader().
func NewLeaderGate(source LeaderSource) *LeaderGate {
	return &LeaderGate{
		source:      source,
		subscribers: make(map[chan bool]struct{}),
		last:        source.IsLeader(),
	}
}

// Owned returns the latest published state — typically reflects the
// raft leadership at the last Publish call.
func (g *LeaderGate) Owned(string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last
}

// Watch subscribes to ownership transitions. The returned channel is
// buffered (size 1) and seeded with the current state so a late
// subscriber still observes the latest value.
func (g *LeaderGate) Watch(string) <-chan bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ch := make(chan bool, 1)
	ch <- g.last
	g.subscribers[ch] = struct{}{}
	return ch
}

// Unwatch removes a subscription. Safe to call with a channel that
// was never registered (e.g. after Close).
func (g *LeaderGate) Unwatch(_ string, ch <-chan bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for sub := range g.subscribers {
		if (<-chan bool)(sub) == ch {
			delete(g.subscribers, sub)
			close(sub)
			return
		}
	}
}

// Publish broadcasts a new ownership state to all subscribers and
// updates the cached last-known value. Idempotent — calling Publish
// with the current value does not re-notify subscribers.
func (g *LeaderGate) Publish(owned bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if owned == g.last {
		return
	}
	g.last = owned
	for sub := range g.subscribers {
		// Drain a stale buffered value so the latest one always
		// lands. Subscribers reading slowly still see the most
		// recent ownership flip rather than a stuck old value.
		select {
		case <-sub:
		default:
		}
		select {
		case sub <- owned:
		default:
		}
	}
}

// Close drops all subscriptions. Subsequent Watch calls still work;
// Publish becomes a no-op for already-closed channels.
func (g *LeaderGate) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for sub := range g.subscribers {
		close(sub)
	}
	g.subscribers = make(map[chan bool]struct{})
}
