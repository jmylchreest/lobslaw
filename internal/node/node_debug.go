package node

import (
	"encoding/json"
	"fmt"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

// debugInspector exposes Node state to agent-side debug builtins.
// Thin accessor layer — the compute package owns tool shape, the
// node owns raw data. Split from node.go because that file is
// already load-bearing.
//
// Every method is nil-safe against subsystems that weren't wired
// on this node (e.g. a compute-only node has no Raft, returns
// an empty/placeholder shape rather than panicking).
type debugInspector struct{ n *Node }

func (d *debugInspector) DebugTools() []string {
	if d.n.toolRegistry == nil {
		return nil
	}
	defs := d.n.toolRegistry.List()
	out := make([]string, 0, len(defs))
	for _, t := range defs {
		out = append(out, t.Name)
	}
	return out
}

func (d *debugInspector) DebugPolicyRules() []string {
	if d.n.store == nil {
		return nil
	}
	var out []string
	_ = d.n.store.ForEach(memory.BucketPolicyRules, func(key string, raw []byte) error {
		var r lobslawv1.PolicyRule
		if err := proto.Unmarshal(raw, &r); err != nil {
			out = append(out, key+" (malformed)")
			return nil
		}
		out = append(out, fmt.Sprintf("%s: %s/%s/%s → %s (priority %d)",
			r.Id, r.Subject, r.Action, r.Resource, r.Effect, r.Priority))
		return nil
	})
	return out
}

func (d *debugInspector) DebugStorageMounts() []string {
	if d.n.storageMgr == nil {
		return nil
	}
	mounts := d.n.storageMgr.List()
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		healthy := "OK"
		if !m.Healthy {
			healthy = "UNHEALTHY"
		}
		out = append(out, fmt.Sprintf("%s [%s] %s (%s)", m.Label, m.Backend, m.Path, healthy))
	}
	return out
}

func (d *debugInspector) DebugMemoryStats() map[string]int {
	if d.n.store == nil {
		return map[string]int{}
	}
	countBucket := func(bucket string) int {
		n := 0
		_ = d.n.store.ForEach(bucket, func(_ string, _ []byte) error {
			n++
			return nil
		})
		return n
	}
	return map[string]int{
		"episodic_records": countBucket(memory.BucketEpisodicRecords),
		"vector_records":   countBucket(memory.BucketVectorRecords),
		"scheduled_tasks":  countBucket(memory.BucketScheduledTasks),
		"commitments":      countBucket(memory.BucketCommitments),
		"policy_rules":     countBucket(memory.BucketPolicyRules),
		"storage_mounts":   countBucket(memory.BucketStorageMounts),
		"audit_entries":    countBucket(memory.BucketAuditEntries),
	}
}

func (d *debugInspector) DebugSoul() string {
	s := d.n.Soul()
	if s == nil {
		return "(no soul loaded)"
	}
	raw, err := json.MarshalIndent(s.Config, "", "  ")
	if err != nil {
		return fmt.Sprintf("(marshal failed: %v)", err)
	}
	return string(raw)
}

func (d *debugInspector) DebugRaft() map[string]any {
	if d.n.raft == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{
		"enabled":        true,
		"node_id":        d.n.cfg.NodeID,
		"is_leader":      d.n.raft.IsLeader(),
		"leader_address": string(d.n.raft.LeaderAddress()),
	}
}

func (d *debugInspector) DebugScheduler() []map[string]any {
	if d.n.store == nil {
		return nil
	}
	var out []map[string]any
	_ = d.n.store.ForEach(memory.BucketScheduledTasks, func(_ string, raw []byte) error {
		var t lobslawv1.ScheduledTaskRecord
		if err := proto.Unmarshal(raw, &t); err != nil {
			return nil
		}
		entry := map[string]any{
			"id":         t.Id,
			"name":       t.Name,
			"schedule":   t.Schedule,
			"handler":    t.HandlerRef,
			"enabled":    t.Enabled,
			"claimed_by": t.ClaimedBy,
		}
		if t.NextRun != nil {
			entry["next_run"] = t.NextRun.AsTime().Format("2006-01-02 15:04:05Z")
		}
		out = append(out, entry)
		return nil
	})
	return out
}

func (d *debugInspector) DebugProviders() []map[string]any {
	providers := d.n.cfg.Compute.Providers
	out := make([]map[string]any, 0, len(providers))
	roles := d.n.cfg.Compute.Roles
	for _, p := range providers {
		var assignedRoles []string
		if roles.Main == p.Label {
			assignedRoles = append(assignedRoles, "main")
		}
		if roles.Preflight == p.Label {
			assignedRoles = append(assignedRoles, "preflight")
		}
		if roles.Reranker == p.Label {
			assignedRoles = append(assignedRoles, "reranker")
		}
		if roles.Summariser == p.Label {
			assignedRoles = append(assignedRoles, "summariser")
		}
		out = append(out, map[string]any{
			"label":      p.Label,
			"endpoint":   p.Endpoint,
			"model":      p.Model,
			"trust_tier": p.TrustTier,
			"roles":      assignedRoles,
		})
	}
	return out
}

func (d *debugInspector) DebugVersion() string {
	return fmt.Sprintf("node_id=%s functions=%v", d.n.cfg.NodeID, d.n.cfg.Functions)
}
