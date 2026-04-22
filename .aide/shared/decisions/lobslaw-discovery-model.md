---
topic: lobslaw-discovery-model
decision: Seed node list provided by user and/or network broadcast for peer discovery. MemoryNodes specifically discover each other to form Raft cluster
date: 2026-04-22
---

# lobslaw-discovery-model

**Decision:** Seed node list provided by user and/or network broadcast for peer discovery. MemoryNodes specifically discover each other to form Raft cluster

## Rationale

Both options gives flexibility: static seeds for controlled environments, broadcast for auto-discovery in local networks

