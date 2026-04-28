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
	// defaultDenyBuiltins = builtins that need an explicit operator
	// allow before the agent can call them. The priority=10 deny
	// seed beats the priority=1 allow seed below; operators open
	// per-scope with an allow rule at priority ≥20.
	//
	// Currently empty. soul_* + oauth_* + credentials_* +
	// clawhub_install moved to noSeedTools (see below) because
	// the stacked deny+allow design tripped models — the LLM saw the
	// deny rule on introspection and refused to call the tool even
	// when an allow existed. research_start was here originally but
	// got the same treatment after operators reported the agent
	// refusing legitimate research requests on policy-blocked
	// systems with no override.
	defaultDenyBuiltins := map[string]bool{}

	// noSeedTools: tools (builtin, skill, or MCP) that get NEITHER
	// a default-allow nor a default-deny seed. The engine returns
	// "default-deny (no rule matched)" when scanned, so strangers
	// can't call them — but an operator [[policy.rules]] allow for
	// a specific scope passes cleanly without fighting a stacked
	// deny seed at priority 10. Used for sensitive owner-scoped
	// tools where "permission lives entirely in the operator
	// config" is the cleanest model. Currently builtin-only; future
	// skills with destructive actions (e.g. clear_workspace) extend
	// this map.
	noSeedTools := map[string]bool{
		"soul_get":              true,
		"soul_tune":             true,
		"soul_fragment_add":     true,
		"soul_fragment_remove":  true,
		"soul_history_rollback": true,
		"oauth_start":           true,
		"oauth_status":          true,
		"oauth_revoke":          true,
		"credentials_grant":     true,
		"credentials_revoke":    true,
		"clawhub_install":       true,
		"binary_install":        true,
		"binary_list":           true,
	}

	// Seed default-allow rules ONLY for builtins (Path prefix
	// BuiltinScheme). Builtins are lobslaw-curated — operators get
	// them by virtue of running the binary, and they have well-
	// understood blast radius. Skills and MCP tools are unbounded
	// (a skill subprocess can do whatever its handler binary
	// allows; an MCP server can call any external API), so they
	// require an explicit operator [[policy.rules]] allow before
	// the agent can call them.
	//
	// Operators express "agent can use minimax for owner scope":
	//
	//   [[policy.rules]]
	//   id       = "owner-allow-minimax"
	//   subject  = "scope:owner"
	//   action   = "tool:exec"
	//   resource = "minimax.*"   # glob; matches all minimax.* tools
	//   effect   = "allow"
	//   priority = 10
	//
	// Skills get the same treatment — explicit allow per skill name.
	seedTargets := []*types.ToolDef{}
	for _, td := range n.toolRegistry.List() {
		if !strings.HasPrefix(td.Path, compute.BuiltinScheme) {
			continue
		}
		if noSeedTools[td.Name] {
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

	// Garbage-collect stale seed rules:
	//   1. Tools now in noSeedTools — both their allow and deny
	//      seeds get removed (operator config only).
	//   2. Tools previously in defaultDenyBuiltins but moved out —
	//      their deny seed must be removed or it keeps fighting the
	//      priority=1 allow seed at scan time. The lobslaw-builtin-deny-*
	//      prefix is the recognisable signature.
	//
	// DELETE via raft so the change replicates across the cluster.
	gcRule := func(id string) {
		if _, err := n.store.Get(memory.BucketPolicyRules, id); err != nil {
			return
		}
		entry := &lobslawv1.LogEntry{
			Op:      lobslawv1.LogOp_LOG_OP_DELETE,
			Id:      id,
			Payload: &lobslawv1.LogEntry_PolicyRule{PolicyRule: &lobslawv1.PolicyRule{Id: id}},
		}
		data, err := proto.Marshal(entry)
		if err != nil {
			n.log.Warn("policy: marshal GC entry failed", "id", id, "err", err)
			return
		}
		if _, err := n.raft.Apply(data, 5*time.Second); err != nil {
			n.log.Warn("policy: GC stale seed rule failed", "id", id, "err", err)
			return
		}
		n.log.Info("policy: removed stale seed rule", "id", id)
	}
	for name := range noSeedTools {
		for _, prefix := range []string{"lobslaw-builtin-", "lobslaw-builtin-deny-"} {
			gcRule(prefix + name)
		}
	}
	for _, td := range seedTargets {
		if defaultDenyBuiltins[td.Name] {
			continue
		}
		gcRule("lobslaw-builtin-deny-" + td.Name)
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

// seedUserPrefsFromConfig writes one BucketUserPrefs record per
// [[user]] entry in operator config, IF the bucket doesn't already
// hold a record with that id. Operator config is the source of
// truth on first boot; runtime edits via builtins win on subsequent
// boots. Leader-only — Put is a raft Apply.
func (n *Node) seedUserPrefsFromConfig(ctx context.Context) error {
	if n.userPrefsSvc == nil || n.raft == nil || !n.raft.IsLeader() {
		return nil
	}
	for _, u := range n.cfg.Users {
		if strings.TrimSpace(u.ID) == "" {
			n.log.Warn("user_prefs: skipping config entry with empty id")
			continue
		}
		if existing, err := n.userPrefsSvc.Get(ctx, u.ID); err == nil && existing != nil {
			continue
		}
		channels := make([]*lobslawv1.UserChannelAddress, 0, len(u.Channels))
		for _, c := range u.Channels {
			channels = append(channels, &lobslawv1.UserChannelAddress{
				Type:    c.Type,
				Address: c.Address,
			})
		}
		rec := &lobslawv1.UserPreferences{
			UserId:      u.ID,
			DisplayName: u.DisplayName,
			Timezone:    u.Timezone,
			Language:    u.Language,
			Channels:    channels,
		}
		if err := n.userPrefsSvc.Put(ctx, rec); err != nil {
			n.log.Warn("user_prefs: seed entry failed", "id", u.ID, "err", err)
			continue
		}
		n.log.Info("user_prefs: seeded from config", "id", u.ID,
			"timezone", u.Timezone, "channels", len(u.Channels))
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
