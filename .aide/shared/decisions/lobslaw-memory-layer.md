---
topic: lobslaw-memory-layer
decision: "Raft+Boltdb for memory node. Vector embeddings + episodic. Dream/REM sleep-like consolidation for long-term memory. MemoryNodes discover each other, form Raft cluster, sync. Single-node Raft if < 3 nodes"
date: 2026-04-22
---

# lobslaw-memory-layer

**Decision:** Raft+Boltdb for memory node. Vector embeddings + episodic. Dream/REM sleep-like consolidation for long-term memory. MemoryNodes discover each other, form Raft cluster, sync. Single-node Raft if < 3 nodes

## Rationale

MemoryNode IS the distributed database. Raft provides consistency. Boltdb is embedded and fast. Episodic memory with consolidation matches biological sleep model

