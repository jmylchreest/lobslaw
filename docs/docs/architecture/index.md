---
sidebar_position: 1
---

# Architecture

How the cluster is built and why.

```
┌─────────────────────────────────────────────────────────────┐
│  cmd/lobslaw                  CLI: init, run, doctor,       │
│                                cluster, plugin, sandbox-exec│
├─────────────────────────────────────────────────────────────┤
│  internal/node                Node lifecycle + wiring.      │
│                                Owns store, agent, gateway,  │
│                                scheduler, egress, …         │
├─────────────────────────────────────────────────────────────┤
│  internal/memory   ┃   internal/compute   ┃  internal/...   │
│  - bolt + raft FSM │   - agent loop       │  - egress       │
│  - episodic        │   - tool registry    │  - skills       │
│  - credentials     │   - builtins         │  - sandbox      │
│  - user_prefs      │   - research         │  - scheduler    │
│  - dreams          │   - context engine   │  - notify       │
├─────────────────────────────────────────────────────────────┤
│  pkg/                                                       │
│  - mtls (atomic.Pointer reload)                             │
│  - crypto (Seal/Open AEAD)                                  │
│  - auth (JWT + JWKS)                                        │
│  - promptgen (per-turn system prompt)                       │
│  - types (Claims, ToolDef, etc.)                            │
│  - proto/lobslaw/v1 (schema)                                │
└─────────────────────────────────────────────────────────────┘
```

## Pages in this section

- [Cluster](/architecture/cluster) — Raft, FSM, leader semantics, mTLS mesh
- [Memory](/architecture/memory) — bolt store, snapshot restore, atomic.Pointer pattern
- [Agent loop](/architecture/agent-loop) — turn lifecycle, tool dispatch, context assembly
- [Discovery](/architecture/discovery) — peer discovery (broadcast, DNS, static)
- [Storage](/architecture/storage) — bolt schema, mounts, watcher

## Design principles

1. **Same binary, single or multi.** The common case is one node; the multi-node case is a config tweak. Either way, every state mutation goes through Raft — a single-node "cluster" is a one-member peer set with immediate commits, not a special code path.
2. **Capability-based, not role-based.** A skill declares what it needs (mounts, networks, credentials); the operator approves or rejects. There are no implicit permissions.
3. **Hot reload everywhere.** Policy rules, mTLS certs, provider configs — SIGHUP, not restart. Restarts mean downtime; downtime means the operator skips reload to avoid it; that means stale config drifts and security regresses.
4. **Honest defaults.** Sensitive built-ins default-deny. Egress default-deny RFC1918. Skills default-deny. The operator opens up what they need.
5. **Audit > prevention.** Every tool call, every policy decision, every credential access is logged. We can't prevent every misuse; we can make sure every misuse is visible after the fact.
