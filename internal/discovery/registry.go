package discovery

import (
	"sort"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Peer bundles the advertised NodeInfo with liveness metadata the
// registry tracks. Peers are in-memory per-node state — not
// Raft-replicated. Re-registration on reconnect is expected.
type Peer struct {
	types.NodeInfo
	LastSeen time.Time
}

// Registry is a concurrent in-memory map of known peers, keyed by
// node ID. It's a superset of the Raft voter configuration: a node
// that runs only compute or gateway functions will appear here but
// not in Raft.
type Registry struct {
	mu    sync.RWMutex
	peers map[types.NodeID]*Peer
	now   func() time.Time // overridable for tests
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		peers: make(map[types.NodeID]*Peer),
		now:   time.Now,
	}
}

// Register adds or updates the peer. LastSeen is set to now. Returns
// true when this was the first time the peer was seen — the broadcast
// listener uses this to trigger an immediate response announce so a
// just-joined node doesn't have to wait a full announce interval to
// discover its neighbours.
func (r *Registry) Register(info types.NodeInfo) (added bool) {
	if info.ID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, existed := r.peers[info.ID]
	r.peers[info.ID] = &Peer{NodeInfo: info, LastSeen: r.now()}
	return !existed
}

// Deregister removes a peer by id. No error when the id is absent —
// deregistration is idempotent.
func (r *Registry) Deregister(id types.NodeID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.peers, id)
}

// Heartbeat bumps a peer's LastSeen. Returns false if the peer isn't
// registered (caller should ask it to re-register).
func (r *Registry) Heartbeat(id types.NodeID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.peers[id]
	if !ok {
		return false
	}
	p.LastSeen = r.now()
	return true
}

// Get returns a peer by id, or (_, false) if absent.
func (r *Registry) Get(id types.NodeID) (Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.peers[id]
	if !ok {
		return Peer{}, false
	}
	return *p, true
}

// List returns a snapshot of all known peers, ordered by ID for
// determinism in tests and logs.
func (r *Registry) List() []Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Peer, 0, len(r.peers))
	for _, p := range r.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// PruneStale removes peers whose LastSeen is older than maxAge.
// Returns the ids that were dropped. Call periodically from a
// background ticker — see Run.
func (r *Registry) PruneStale(maxAge time.Duration) []types.NodeID {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := r.now().Add(-maxAge)
	var dropped []types.NodeID
	for id, p := range r.peers {
		if p.LastSeen.Before(cutoff) {
			dropped = append(dropped, id)
			delete(r.peers, id)
		}
	}
	return dropped
}
