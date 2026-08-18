package node

import (
	"context"
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/singleton"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// wireSelfTaught constructs the store the agent writes its own
// instructions into — or does not.
//
// With mode = "off" this returns nil having built nothing, so
// n.selfTaught stays nil and every dependent is ABSENT rather than
// guarded. That distinction is the whole point: "the capability is not
// present" is a different and stronger claim than "the call sites
// check a flag", and the second is not what an operator disabling
// self-learning is asking for.
func (n *Node) wireSelfTaught() error {
	mode := memory.ParseSelfLearningMode(n.cfg.SelfLearningMode)
	if mode == memory.SelfLearningOff {
		// Said out loud at boot. Silence here is indistinguishable
		// from a wiring bug, and an operator who meant to enable it
		// should find out now rather than by wondering why nothing
		// was ever learned.
		n.log.Info("self-learning: disabled; the store is not wired and no artefact can be written")
		return nil
	}

	store, err := memory.NewSelfTaughtStore(n.raft, n.store, mode)
	if err != nil {
		return fmt.Errorf("self-taught store: %w", err)
	}
	store.SetLimits(n.cfg.SelfTaughtMaxFileBytes, n.cfg.SelfTaughtMaxTotalBytes,
		n.cfg.SelfTaughtHistoryDepth)
	n.selfTaught = store
	n.log.Info("self-learning: enabled", "mode", string(mode),
		"artefacts_active_immediately", mode == memory.SelfLearningAuto)

	// Registered here rather than unconditionally in the subsystem
	// stage, so a cluster with self-learning off does not expose an
	// RPC surface for a store that does not exist. Absence, not a
	// guarded call — the same property the store itself has.
	if n.server != nil {
		lobslawv1.RegisterSelfLearningServiceServer(n.server,
			&selfLearningService{store: store})
	}

	// The in-channel nudge. Built here rather than in the gateway
	// stage because it needs the store.
	//
	// The audience is DECIDED before it reaches this struct — propose
	// mode defaults it on, derived from the configured channels and
	// the owner-scoped users, unless notify.disabled says otherwise.
	// See resolveNoticeAudience in cmd/lobslaw. Empty here now means "resolved
	// to nobody", not "nobody typed a list".
	n.notices = gateway.NewNotices(pendingReviewSource{store: store}, gateway.NoticeConfig{
		Channels: n.cfg.NotifyChannels,
		Subjects: n.cfg.NotifySubjects,
		Interval: n.cfg.NotifyInterval,
	})
	if len(n.cfg.NotifyChannels) > 0 && len(n.cfg.NotifySubjects) > 0 {
		n.log.Info("self-learning: review notices enabled",
			"channels", n.cfg.NotifyChannels, "subjects", len(n.cfg.NotifySubjects))
	} else {
		// Said out loud, because "I never got told about the queue" is
		// otherwise indistinguishable from "the queue was empty" —
		// and with proposal expiry running, the difference matters.
		n.log.Info("self-learning: review notices are off",
			"reason", "self_learning.notify needs both channels and subjects")
	}
	return nil
}

// curatorName is the singleton key for the self-taught lifecycle pass.
const curatorName = "self-taught-curator"

// startCurator ages unused artefacts out of the live set on whichever
// node holds leadership.
//
// Leader-pinned rather than per-node. Every node running it would be
// correct — the transitions go through raft and are idempotent — but
// it would burn a round trip per node per transition to reach the same
// answer. That is the opposite of the materialiser, which every node
// must run because a cache is per-node and a lifecycle is not.
func (n *Node) startCurator(ctx context.Context) {
	if n.selfTaught == nil || n.leaderGate == nil {
		return
	}
	cfg := memory.CuratorConfig{
		StaleAfterDays:     n.cfg.SelfTaughtStaleAfterDays,
		ArchiveAfterDays:   n.cfg.SelfTaughtArchiveAfterDays,
		ProposalExpiryDays: n.cfg.SelfTaughtProposalExpiryDays,
	}
	go func() {
		err := singleton.Run(ctx, n.leaderGate, curatorName, n.log,
			func(ctx context.Context) error {
				return n.selfTaught.CurateLoop(ctx, cfg, n.log)
			})
		if err != nil && ctx.Err() == nil {
			n.log.Warn("self-taught curator stopped", "err", err)
		}
	}()
}

// wireReviewFork builds the post-turn review.
//
// Returns nil having built nothing when there is no self-taught store,
// which is the same absence-not-a-flag property: with self-learning
// off there is no fork to disable, because there was never anywhere
// for it to write.
func (n *Node) wireReviewFork() error {
	if n.selfTaught == nil || n.roleMap == nil {
		return nil
	}
	fork, err := compute.NewReviewFork(compute.ReviewConfig{
		Roles:               n.roleMap,
		Store:               artefactStoreAdapter{inner: n.selfTaught},
		Logger:              n.log,
		SkillToolIterations: n.cfg.ReviewSkillToolIterations,
		MemoryTurnInterval:  n.cfg.ReviewMemoryTurnInterval,
	})
	if err != nil {
		return fmt.Errorf("review fork: %w", err)
	}
	n.reviewFork = fork
	if n.agent != nil {
		n.agent.SetReview(fork)
	}
	n.log.Info("review fork: enabled",
		"skill_tool_iterations", n.cfg.ReviewSkillToolIterations,
		"memory_turn_interval", n.cfg.ReviewMemoryTurnInterval)
	return nil
}
