package node

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
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
	stats := d.n.raft.Raft.Stats()
	leaderAddr, leaderID := d.n.raft.Raft.LeaderWithID()

	// "caught_up" is true when this node has applied every entry the
	// cluster has committed — the operator-facing answer to "has
	// replication finished?". Followers lag by one commit cycle even
	// in steady state, so a small applied < commit gap is normal
	// momentarily; a large or growing gap is what to worry about.
	commit := atoiSafe(stats["commit_index"])
	applied := atoiSafe(stats["applied_index"])
	lastLog := atoiSafe(stats["last_log_index"])

	servers := []map[string]string{}
	if cfg := d.n.raft.ConfigurationServers(); cfg != nil {
		for _, s := range cfg {
			servers = append(servers, map[string]string{
				"id":       string(s.ID),
				"address":  string(s.Address),
				"suffrage": s.Suffrage.String(),
			})
		}
	}

	return map[string]any{
		"enabled":        true,
		"node_id":        d.n.cfg.NodeID,
		"state":          stats["state"],
		"term":           stats["term"],
		"is_leader":      d.n.raft.IsLeader(),
		"leader_id":      string(leaderID),
		"leader_address": string(leaderAddr),
		"last_log_index": lastLog,
		"commit_index":   commit,
		"applied_index":  applied,
		"applied_lag":    commit - applied,
		"caught_up":      applied >= commit,
		"last_contact":   stats["last_contact"],
		"num_peers":      stats["num_peers"],
		"servers":        servers,
	}
}

func atoiSafe(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
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

// DebugSandbox probes live kernel capabilities. See
// internal/sandbox.Probe for what's surfaced; this wrapper turns
// the typed report into the map[string]any shape DebugInspector
// expects.
func (d *debugInspector) DebugSandbox() map[string]any {
	r := sandbox.Probe()
	return map[string]any{
		"os":                     r.OS,
		"kernel_version":         r.KernelVersion,
		"landlock_supported":     r.LandlockSupported,
		"landlock_abi_version":   r.LandlockABIVersion,
		"seccomp_supported":      r.SeccompSupported,
		"no_new_privs_supported": r.NoNewPrivsSupported,
		"cgroup_v2_mounted":      r.CgroupV2Mounted,
		"daemon_under_sandbox":   r.DaemonUnderSandbox,
		"sandbox_mode":           r.SandboxMode,
	}
}

// DebugMCP exposes the live MCP registry: configured-vs-live counts,
// per-server command/args/install spec and the tools each server
// contributed. Tools are listed by their qualified name (e.g.
// "minimax.text_to_image") so an agent answering "why isn't X
// available?" can confirm whether the upstream server is even live.
func (d *debugInspector) DebugMCP() map[string]any {
	configured := []map[string]any{}
	for name, s := range d.n.cfg.MCP.Servers {
		entry := map[string]any{
			"name":     name,
			"command":  s.Command,
			"args":     s.Args,
			"disabled": s.Disabled,
		}
		if len(s.Install) > 0 {
			entry["install"] = s.Install
		}
		configured = append(configured, entry)
	}

	live := []map[string]any{}
	if d.n.mcpLoader != nil {
		toolsByServer := map[string][]string{}
		for _, t := range d.n.mcpLoader.ListTools() {
			parts := strings.SplitN(t.Name, ".", 2)
			if len(parts) == 2 {
				toolsByServer[parts[0]] = append(toolsByServer[parts[0]], t.Name)
			}
		}
		for _, srv := range d.n.mcpLoader.ListServers() {
			tools := toolsByServer[srv.Name]
			sort.Strings(tools)
			live = append(live, map[string]any{
				"name":       srv.Name,
				"command":    srv.Command,
				"args":       srv.Args,
				"tool_count": srv.ToolCount,
				"healthy":    srv.Healthy,
				"tools":      tools,
			})
		}
	}

	return map[string]any{
		"configured_count": len(configured),
		"live_count":       len(live),
		"configured":       configured,
		"live":             live,
	}
}
