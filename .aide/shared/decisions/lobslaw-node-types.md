---
topic: lobslaw-node-types
decision: "Four node functions: Memory (vector+episodic, Raft+Boltdb), Policy (RBAC+rules+audit+scheduled tasks, same Raft group as memory for cluster metadata), Compute (agent core, LLM, tools, rclone mounts, sidecars), Gateway (gRPC server, channel handlers, auth). Storage is not a node - shared storage is an rclone mount owned by Compute. Single binary enables any subset per deployment"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-node-types

**Decision:** Four node functions: Memory (vector+episodic, Raft+Boltdb), Policy (RBAC+rules+audit+scheduled tasks, same Raft group as memory for cluster metadata), Compute (agent core, LLM, tools, rclone mounts, sidecars), Gateway (gRPC server, channel handlers, auth). Storage is not a node - shared storage is an rclone mount owned by Compute. Single binary enables any subset per deployment

## Rationale

Original listed StorageNode (object+FUSE) as a separate node type. Subsequent lobslaw-storage-model collapsed storage into a compute-owned rclone mount. This supersede aligns the node-types decision with the storage-model decision - four nodes, not five, storage-as-mount-not-node

