package discovery

import (
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestRegistryRegisterAndList(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(types.NodeInfo{ID: "a", Address: "10.0.0.1:9090"})
	r.Register(types.NodeInfo{ID: "b", Address: "10.0.0.2:9090"})
	r.Register(types.NodeInfo{ID: "c", Address: "10.0.0.3:9090"})

	peers := r.List()
	if len(peers) != 3 {
		t.Fatalf("want 3 peers, got %d", len(peers))
	}
	// List returns sorted by ID.
	want := []types.NodeID{"a", "b", "c"}
	for i, p := range peers {
		if p.ID != want[i] {
			t.Errorf("peer[%d].ID = %q, want %q", i, p.ID, want[i])
		}
	}
}

func TestRegistryRegisterUpdatesLastSeen(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	past := time.Unix(1_700_000_000, 0)
	now := past.Add(time.Hour)
	r.now = func() time.Time { return past }
	r.Register(types.NodeInfo{ID: "a"})
	p, _ := r.Get("a")
	if !p.LastSeen.Equal(past) {
		t.Errorf("LastSeen = %v, want %v", p.LastSeen, past)
	}

	r.now = func() time.Time { return now }
	r.Register(types.NodeInfo{ID: "a"}) // re-register
	p, _ = r.Get("a")
	if !p.LastSeen.Equal(now) {
		t.Errorf("re-register LastSeen = %v, want %v", p.LastSeen, now)
	}
}

func TestRegistryRejectsEmptyID(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(types.NodeInfo{ID: "", Address: "nope"})
	if got := len(r.List()); got != 0 {
		t.Errorf("want 0 peers, got %d", got)
	}
}

func TestRegistryDeregisterIdempotent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.Register(types.NodeInfo{ID: "a"})
	r.Deregister("a")
	r.Deregister("a") // again; must not panic
	r.Deregister("never-registered")
	if got := len(r.List()); got != 0 {
		t.Errorf("want 0 peers, got %d", got)
	}
}

func TestRegistryHeartbeat(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	past := time.Unix(1_700_000_000, 0)
	later := past.Add(time.Minute)
	r.now = func() time.Time { return past }
	r.Register(types.NodeInfo{ID: "a"})

	r.now = func() time.Time { return later }
	if !r.Heartbeat("a") {
		t.Error("Heartbeat should return true for registered peer")
	}
	p, _ := r.Get("a")
	if !p.LastSeen.Equal(later) {
		t.Errorf("LastSeen after heartbeat = %v, want %v", p.LastSeen, later)
	}

	if r.Heartbeat("unknown") {
		t.Error("Heartbeat should return false for unregistered peer")
	}
}

func TestRegistryPruneStale(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	r.Register(types.NodeInfo{ID: "old-1"})
	r.Register(types.NodeInfo{ID: "old-2"})

	// Move time forward past the cutoff for old-1 and old-2, then add fresh.
	r.now = func() time.Time { return base.Add(10 * time.Minute) }
	r.Register(types.NodeInfo{ID: "fresh"})

	dropped := r.PruneStale(5 * time.Minute)
	if len(dropped) != 2 {
		t.Errorf("want 2 dropped, got %d (%v)", len(dropped), dropped)
	}
	if _, ok := r.Get("fresh"); !ok {
		t.Error("fresh peer should survive prune")
	}
	if _, ok := r.Get("old-1"); ok {
		t.Error("old-1 should have been pruned")
	}
}
