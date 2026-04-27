package node

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/singleton"
	"github.com/jmylchreest/lobslaw/pkg/rafttransport"
)

func (n *Node) wireRaft(advertise string) error {
	store, err := memory.OpenStore(filepath.Join(n.cfg.DataDir, "state.db"), n.cfg.MemoryKey)
	if err != nil {
		return fmt.Errorf("open state.db: %w", err)
	}
	fsm := memory.NewFSM(store)

	transport, err := rafttransport.New(rafttransport.Config{
		LocalAddr: raft.ServerAddress(advertise),
		DialOpts:  []grpc.DialOption{grpc.WithTransportCredentials(n.cfg.Creds.ClientCreds())},
	})
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("rafttransport.New: %w", err)
	}
	transport.Register(n.server)

	rNode, err := memory.NewRaft(memory.RaftConfig{
		NodeID:    n.cfg.NodeID,
		LocalAddr: raft.ServerAddress(advertise),
		DataDir:   n.cfg.DataDir,
		// Bootstrap is decided by the orchestration in Start() after
		// the gRPC listener is up and a join attempt has run; we
		// never let NewRaft auto-bootstrap a fresh cluster.
		Bootstrap: false,
		Transport: transport.RaftTransport(),
		Logger:    n.log,
	}, fsm)
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("memory.NewRaft: %w", err)
	}

	n.store = store
	n.fsm = fsm
	n.transport = transport
	n.raft = rNode

	// Leader-pinned singleton coordinator. Constructed here because
	// it needs the raft handle to seed initial state and to receive
	// transitions. Workloads that want exactly-one-owner semantics
	// (telegram polling today; future: dream cycles, reindex) take
	// this gate from n.leaderGate.
	n.leaderGate = singleton.NewLeaderGate(rNode)
	rNode.SetLeadershipCallback(func(isLeader bool) {
		n.leaderGate.Publish(isLeader)
		// Wake the scheduler immediately on leadership gain so the
		// new leader doesn't sleep MaxSleep before scanning past-due
		// records. Followers' scheduler loops sleep MaxSleep — without
		// this nudge they'd miss the chance to fire anything that was
		// already overdue at the moment of promotion.
		if isLeader && n.scheduler != nil {
			n.scheduler.Notify()
		}
	})
	return nil
}

// establishRaftMembership runs the bootstrap/join decision tree.
// (1) Existing state + we're in config → resume.
// (2) Existing state + we're NOT in config → fail-fast (orphan; data
// dir came from a different identity). (3) No state + seeds → try
// JoinCluster (AddMember w/ leader redirect) until BootstrapTimeout,
// then WaitForConfigInclusion. (4) No state + no/failed seeds +
// cfg.Bootstrap → solo bootstrap. (5) No state + cfg.Bootstrap=false
// → fail-fast. Cases 4/5 protect against split-brain: production
// joiners run with Bootstrap=false so a startup-time partition
// can't create two independent clusters.
func (n *Node) establishRaftMembership(ctx context.Context) error {
	timeout := n.cfg.BootstrapTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	if n.raft.HadStateOnBoot() {
		if n.raft.IsInConfiguration() {
			n.log.Info("raft: resuming with existing state",
				"node_id", n.cfg.NodeID,
				"servers", formatServers(n.raft.ConfigurationServers()))
			return nil
		}
		return fmt.Errorf("raft data dir at %q has prior state but does not list this node (%q) as a voter — current servers: %v. "+
			"this typically means the data dir was carried over from a different node identity; "+
			"either point at a clean data dir, or wipe raft.db + snapshots/ to start fresh",
			n.cfg.DataDir, n.cfg.NodeID, formatServers(n.raft.ConfigurationServers()))
	}

	// Virgin data dir from here on. Combine the operator-supplied
	// seed list with anything the broadcaster has heard so far —
	// either source is enough to find a peer to dial. Brief wait
	// (≤2s) gives the broadcast listener a chance to populate the
	// registry on container/LAN startups where everyone races up.
	candidates := n.collectJoinCandidates(ctx, 2*time.Second)
	if len(candidates) > 0 {
		joinCtx, cancel := context.WithTimeout(ctx, timeout)
		err := n.discCli.JoinCluster(joinCtx, candidates, 5*time.Second)
		cancel()
		if err == nil {
			waitCtx, waitCancel := context.WithTimeout(ctx, timeout)
			defer waitCancel()
			if err := n.raft.WaitForConfigInclusion(waitCtx, 200*time.Millisecond); err != nil {
				return fmt.Errorf("joined via candidates but never observed self in committed config within %s: %w", timeout, err)
			}
			n.log.Info("raft: joined existing cluster",
				"node_id", n.cfg.NodeID,
				"candidates", candidates)
			return nil
		}
		n.log.Warn("raft: join via discovered candidates failed",
			"err", err,
			"candidates", candidates,
			"will_bootstrap", n.cfg.Bootstrap)
	}

	if !n.cfg.Bootstrap {
		return fmt.Errorf("no existing raft state, no seed or broadcast peer reachable, and bootstrap=false — refusing to form a fresh cluster on my own. "+
			"either set [discovery] seed_nodes / broadcast=true with a reachable peer, or set [cluster] bootstrap=true to allow solo-bootstrap (candidates=%v)",
			candidates)
	}

	if err := n.raft.BootstrapSelf(); err != nil {
		return fmt.Errorf("solo bootstrap: %w", err)
	}
	n.log.Info("raft: bootstrapped a new cluster as sole voter",
		"node_id", n.cfg.NodeID,
		"reason", bootstrapReason(candidates))
	return nil
}

func bootstrapReason(candidates []string) string {
	if len(candidates) == 0 {
		return "no seeds configured and no broadcast peer heard"
	}
	return "all reachable candidates rejected the join (likely cold-start race; bringing nodes up sequentially avoids this)"
}

// collectJoinCandidates returns the merged set of operator-supplied
// seed_nodes and broadcast-discovered peer addresses. When the
// broadcaster is enabled, sleeps up to waitFor to give the listener
// a chance to populate the registry — typical announce interval is
// 30s but the first packet usually arrives within 1-2s of boot.
// Self is excluded; duplicates are de-duped while preserving order
// (operator seeds first, then discovered peers).
func (n *Node) collectJoinCandidates(ctx context.Context, waitFor time.Duration) []string {
	if n.broadcaster != nil && waitFor > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(waitFor):
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, s := range n.cfg.SeedNodes {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if n.registry != nil {
		for _, p := range n.registry.List() {
			if string(p.ID) == n.cfg.NodeID {
				continue
			}
			if p.Address == "" || seen[p.Address] {
				continue
			}
			seen[p.Address] = true
			out = append(out, p.Address)
		}
	}
	return out
}

func formatServers(servers []raft.Server) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		out = append(out, fmt.Sprintf("%s@%s(%s)", s.ID, s.Address, s.Suffrage))
	}
	return out
}
