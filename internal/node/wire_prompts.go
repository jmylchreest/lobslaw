package node

import (
	"context"
	"fmt"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/singleton"
)

// promptSweeperName is the singleton key for the expiry sweeper.
const promptSweeperName = "prompt-sweeper"

// wirePrompts picks the confirmation registry this node can actually
// back, and starts the expiry sweeper if it hosts raft.
//
// A gateway on a compute-only node has no local raft, so it keeps the
// in-memory registry: same behaviour as before, confined to one
// process. Anywhere raft is present, a confirmation issued here can be
// answered on a peer and survives this process restarting.
func (n *Node) wirePrompts() error {
	if n.raft == nil || n.store == nil {
		n.log.Info("prompts: no local raft; confirmations are process-local",
			"reason", "gateway on a node without the memory function")
		n.promptRegistry = gateway.NewPromptRegistry()
		return nil
	}

	store, err := memory.NewPromptStore(memory.PromptStoreConfig{
		Raft:  n.raft,
		Store: n.store,
		Log:   n.log,
	})
	if err != nil {
		return fmt.Errorf("prompt store: %w", err)
	}
	// The caps come from this node's current config, so a turn that
	// paused before an operator lowered a limit resumes under the new
	// one rather than the old.
	n.promptRegistry = gateway.NewRaftPrompts(store, n.cfg.NodeID,
		compute.FromComputeConfig(n.cfg.Compute))
	n.promptStore = store

	rules, err := policy.NewApprovalRules(n.raft, n.store)
	if err != nil {
		return fmt.Errorf("approval rules: %w", err)
	}
	n.approvalRules = rules

	grants, err := memory.NewSessionGrantStore(n.raft, n.store, n.cfg.SessionGrantTTL)
	if err != nil {
		return fmt.Errorf("session grants: %w", err)
	}
	n.sessionGrants = grants
	// The executor's approval store is built in the compute stage,
	// which runs BEFORE this one — so the durable half is attached
	// here rather than passed in at construction. Attaching is the
	// right shape anyway: a node with no local raft reaches this
	// function's early return and keeps a process-local map, which is
	// the behaviour it had before rather than a silently missing
	// feature.
	if n.approvals != nil {
		n.approvals.SetDurable(grantsAdapter{inner: grants})
		n.log.Info("session grants: replicated", "ttl", grants.TTL())
	}
	// And to the policy service, which serves the listing and the
	// revoke. Without this the two RPCs report Unimplemented, which is
	// the honest answer for a node with no replicated store and the
	// wrong one for a node that has it and forgot to say so.
	if n.policySvc != nil {
		n.policySvc.SetSessionGrants(grants)
	}
	return nil
}

// startPromptSweeper closes out expired confirmations on whichever
// node holds leadership. Leader-pinned rather than per-node because
// every node sweeping would be correct but would burn a raft
// round-trip per node per expiry.
func (n *Node) startPromptSweeper(ctx context.Context) {
	if n.promptStore == nil || n.leaderGate == nil {
		return
	}
	go func() {
		err := singleton.Run(ctx, n.leaderGate, promptSweeperName, n.log,
			func(ctx context.Context) error {
				return n.promptStore.SweepLoop(ctx, memory.DefaultSweepInterval)
			})
		if err != nil && ctx.Err() == nil {
			n.log.Warn("prompt sweeper stopped", "err", err)
		}
	}()
}

// grantSweeperName is the singleton key for the expired-grant sweep.
const grantSweeperName = "session-grant-sweeper"

// startGrantSweeper removes expired conversation grants on whichever
// node holds leadership.
//
// Hygiene rather than enforcement: Granted checks expiry on every
// read, so a grant is dead the moment it expires whether or not this
// has run. What it buys is a bucket that does not accumulate one dead
// record per confirmation ever answered — which over a year is the
// difference between a snapshot and a problem.
//
// Hourly, because nothing depends on its promptness. A sweep whose
// lateness could make a grant live longer would need to be much more
// frequent, and would still be the wrong design.
func (n *Node) startGrantSweeper(ctx context.Context) {
	if n.sessionGrants == nil || n.leaderGate == nil {
		return
	}
	go func() {
		err := singleton.Run(ctx, n.leaderGate, grantSweeperName, n.log,
			func(ctx context.Context) error {
				return n.sessionGrants.SweepLoop(ctx, time.Hour, n.log)
			})
		if err != nil && ctx.Err() == nil {
			n.log.Warn("session grant sweeper stopped", "err", err)
		}
	}()
}

// wirePinnedMemory constructs the always-on memory store.
//
// Its own raft-gated stage rather than part of the gateway wiring,
// because the compute stage registers the tools that use it and
// compute runs first — constructing it later would leave the tools
// silently absent on every node.
func (n *Node) wirePinnedMemory() error {
	pinned, err := memory.NewPinnedStore(n.raft, n.store, memory.PinnedConfig{
		ProfileCap: n.cfg.MemoryPinnedProfileChars,
		NotesCap:   n.cfg.MemoryPinnedNotesChars,
	})
	if err != nil {
		return fmt.Errorf("pinned memory: %w", err)
	}
	n.pinnedStore = pinned

	// Dream tidies these blocks when they near their cap, so the
	// pressure produces curation in the background rather than a write
	// failure the user sees. Wired here rather than at Dream's
	// construction because the store is built later in the boot order
	// than the runner is.
	if n.memorySvc != nil {
		if runner := n.memorySvc.DreamRunner(); runner != nil {
			runner.SetPinnedStore(pinned)
		}
	}
	return nil
}
