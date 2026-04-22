---
topic: lobslaw-persistence-model
decision: "Two distinct persistent layers: Memory (Raft+Boltdb, agent context + episodic) and Storage (Object store, skills + general data). Skills merge order configurable. No badger in scope"
date: 2026-04-22
---

# lobslaw-persistence-model

**Decision:** Two distinct persistent layers: Memory (Raft+Boltdb, agent context + episodic) and Storage (Object store, skills + general data). Skills merge order configurable. No badger in scope

## Rationale

Clean separation: memory is for agent state/context, storage is for persistent objects. Skills live in storage layer

