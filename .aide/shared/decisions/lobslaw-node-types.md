---
topic: lobslaw-node-types
decision: "Five node functions: Memory (vector+episodic+retention, Raft+bbolt), Policy (RBAC+rules+audit+scheduled tasks, same Raft group as memory for cluster metadata), Compute (agent core, LLM, tools, hooks, sidecars), Gateway (gRPC server, channel handlers, auth, inline confirmation), Storage (rclone/local/nfs mount lifecycle, unified Watcher, namespace sandboxing). Single binary enables any subset per deployment. Single-agent mode enables all five. Memory must be co-located with Storage so snapshot-export targets resolve locally (enforced at startup)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-node-types

**Decision:** Five node functions: Memory (vector+episodic+retention, Raft+bbolt), Policy (RBAC+rules+audit+scheduled tasks, same Raft group as memory for cluster metadata), Compute (agent core, LLM, tools, hooks, sidecars), Gateway (gRPC server, channel handlers, auth, inline confirmation), Storage (rclone/local/nfs mount lifecycle, unified Watcher, namespace sandboxing). Single binary enables any subset per deployment. Single-agent mode enables all five. Memory must be co-located with Storage so snapshot-export targets resolve locally (enforced at startup)

## Rationale

Earlier four-function model put mount management under Compute, which broke snapshot-export for memory-only nodes and couldn't propagate mount config cluster-wide. Promoting Storage to a first-class function fixes both. See lobslaw-storage-model for the storage function's specific design

