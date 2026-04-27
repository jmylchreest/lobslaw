package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (n *Node) seedDefaultPolicyRules(ctx context.Context) error {
	if n.raft == nil || n.store == nil || n.policySvc == nil {
		return nil
	}
	if !n.raft.IsLeader() {
		return nil
	}
	// Seed for every registered builtin — includes web_search when
	// Exa is configured, plus any future builtin registrations. We
	// iterate the live registry rather than the static StdlibToolDefs
	// so additive wiring in node.New doesn't need to update a second
	// list here.
	// A Raft-hosting node without the compute function has no
	// tool registry — nothing to seed. Skip cleanly.
	if n.toolRegistry == nil {
		return nil
	}
	// defaultDenyBuiltins = builtins that need explicit operator
	// allow before the agent can call them. Their priority>1 deny
	// seed beats the priority=1 allow seed below; operators add an
	// allow rule (any priority ≥2) to enable per scope.
	// Default-deny builtins: get a priority=10 deny seed at boot.
	// Operators open per-scope by adding an [[policy.rules]] allow
	// entry at higher priority (typically 20+).
	//
	// soul_* tools were here originally but moved out: the deny
	// seed keeps tripping models that reason about the rule before
	// trying the call ("there's a deny rule, so I won't try"). The
	// owner-scope allow rule is the ONLY guard now — strangers
	// without scope:owner claims still can't call them because the
	// allow rule's subject won't match. A/B test for whether the
	// stacked-deny+allow design was making models over-cautious.
	defaultDenyBuiltins := map[string]bool{
		"research_start": true,
	}

	// noSeedBuiltins: tools that get NEITHER a default-allow nor a
	// default-deny seed. The engine returns "default-deny (no rule
	// matched)" when scanned, so strangers can't call them — but
	// an operator [[policy.rules]] allow for a specific scope
	// passes cleanly without fighting a stacked deny seed at
	// priority 10. Used for sensitive owner-scoped tools where
	// "permission lives entirely in the operator config" is the
	// cleanest model.
	noSeedBuiltins := map[string]bool{
		"soul_get":              true,
		"soul_tune":             true,
		"soul_fragment_add":     true,
		"soul_fragment_remove":  true,
		"soul_fragment_list":    true,
		"soul_history_rollback": true,
	}

	seedTargets := []*types.ToolDef{}
	for _, td := range n.toolRegistry.List() {
		if !strings.HasPrefix(td.Path, compute.BuiltinScheme) {
			continue
		}
		if noSeedBuiltins[td.Name] {
			continue
		}
		seedTargets = append(seedTargets, td)
	}
	seeded := []string{}
	for _, td := range seedTargets {
		effect := "allow"
		priority := int32(1)
		ruleID := "lobslaw-builtin-" + td.Name
		if defaultDenyBuiltins[td.Name] {
			effect = "deny"
			priority = 10
			ruleID = "lobslaw-builtin-deny-" + td.Name
		}
		if _, err := n.store.Get(memory.BucketPolicyRules, ruleID); err == nil {
			// Already present (prior boot seeded it, or operator
			// wrote a rule with this ID explicitly). Skip.
			continue
		}
		_, err := n.policySvc.AddRule(ctx, &lobslawv1.AddRuleRequest{
			Rule: &lobslawv1.PolicyRule{
				Id:       ruleID,
				Subject:  "*",
				Action:   "tool:exec",
				Resource: td.Name,
				Effect:   effect,
				Priority: priority,
			},
		})
		if err != nil {
			return fmt.Errorf("seed %q: %w", ruleID, err)
		}
		n.log.Debug("policy: seeded default builtin rule",
			"tool", td.Name, "rule_id", ruleID, "effect", effect)
		seeded = append(seeded, td.Name)
	}
	if len(seeded) > 0 {
		n.log.Info("policy: seeded default builtin rules", "count", len(seeded))
	}

	// Garbage-collect stale seed rules for tools now in noSeedBuiltins.
	// Without this, a tool that was previously default-allow- or
	// default-deny-seeded would keep its seed rule forever even after
	// being moved to "operator-config-only" semantics, and that stale
	// seed could fight the operator's allow rule (e.g. priority 10
	// deny still influences the scan). DELETE via raft so the change
	// replicates.
	for name := range noSeedBuiltins {
		for _, prefix := range []string{"lobslaw-builtin-", "lobslaw-builtin-deny-"} {
			id := prefix + name
			if _, err := n.store.Get(memory.BucketPolicyRules, id); err != nil {
				continue
			}
			entry := &lobslawv1.LogEntry{
				Op:      lobslawv1.LogOp_LOG_OP_DELETE,
				Id:      id,
				Payload: &lobslawv1.LogEntry_PolicyRule{PolicyRule: &lobslawv1.PolicyRule{Id: id}},
			}
			data, err := proto.Marshal(entry)
			if err != nil {
				n.log.Warn("policy: marshal GC entry failed", "id", id, "err", err)
				continue
			}
			if _, err := n.raft.Apply(data, 5*time.Second); err != nil {
				n.log.Warn("policy: GC stale seed rule failed", "id", id, "err", err)
				continue
			}
			n.log.Info("policy: removed stale seed rule (tool now operator-config-only)",
				"id", id)
		}
	}

	// Operator-declared [[policy.rules]] from config.toml. These get
	// seeded with the operator-supplied ID — re-seed is a no-op on
	// repeat boots because we skip when the bucket already has the
	// rule. Operator-edited rules requires explicit re-apply (delete
	// the rule first, restart) to avoid silently overwriting.
	for _, r := range n.cfg.Policy.Rules {
		if r.ID == "" {
			n.log.Warn("policy: skipping operator rule without id")
			continue
		}
		if _, err := n.store.Get(memory.BucketPolicyRules, r.ID); err == nil {
			continue
		}
		_, err := n.policySvc.AddRule(ctx, &lobslawv1.AddRuleRequest{
			Rule: &lobslawv1.PolicyRule{
				Id:       r.ID,
				Subject:  r.Subject,
				Action:   r.Action,
				Resource: r.Resource,
				Effect:   r.Effect,
				Priority: r.Priority,
			},
		})
		if err != nil {
			return fmt.Errorf("seed operator rule %q: %w", r.ID, err)
		}
		n.log.Info("policy: seeded operator rule",
			"id", r.ID, "subject", r.Subject, "action", r.Action,
			"resource", r.Resource, "effect", r.Effect, "priority", r.Priority)
	}
	return nil
}

// seedDreamTask installs a recurring memory:dream task in the
// scheduler if one isn't already present under the well-known
// "lobslaw-builtin-dream" ID. Default cadence: 02:00 daily. Operator
// turns it off via [memory.dream] enabled = false. Operators who
// want a different cadence override [memory.dream] schedule, or
// declare their own [[scheduler.tasks]] (different ID) for parallel
// passes. Idempotent — won't overwrite an existing entry, so a
// schedule change requires deleting the seeded task first (which
// the next boot will then re-seed at the new cadence).
func (n *Node) seedDreamTask(ctx context.Context) error {
	if n.raft == nil || n.store == nil || n.scheduler == nil || n.memorySvc == nil {
		return nil
	}
	if !n.raft.IsLeader() {
		return nil
	}
	// Operator opt-out. *bool default-nil → enabled.
	if n.cfg.MemoryDream.Enabled != nil && !*n.cfg.MemoryDream.Enabled {
		return nil
	}
	const dreamTaskID = "lobslaw-builtin-dream"
	if _, err := n.store.Get(memory.BucketScheduledTasks, dreamTaskID); err == nil {
		return nil
	}
	schedule := strings.TrimSpace(n.cfg.MemoryDream.Schedule)
	if schedule == "" {
		schedule = "0 2 * * *"
	}
	task := &lobslawv1.ScheduledTaskRecord{
		Id:         dreamTaskID,
		Name:       "memory.dream (builtin)",
		Schedule:   schedule,
		HandlerRef: DreamHandlerRef,
		Enabled:    true,
		CreatedAt:  timestamppb.Now(),
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      dreamTaskID,
		Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: task},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal dream task: %w", err)
	}
	if _, err := n.raft.Apply(data, 5*time.Second); err != nil {
		return fmt.Errorf("apply dream task: %w", err)
	}
	n.log.Info("memory: seeded dream task", "id", dreamTaskID, "schedule", schedule)
	return nil
}
