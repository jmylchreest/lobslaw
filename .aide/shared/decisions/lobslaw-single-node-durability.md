---
topic: lobslaw-single-node-durability
decision: Single-node Raft deployments must configure periodic Raft snapshot export to a configured storage backend. Not optional - startup fails if enabled without a snapshot target configured. Snapshot cadence defaults to hourly; retention configurable
date: 2026-04-22
---

# lobslaw-single-node-durability

**Decision:** Single-node Raft deployments must configure periodic Raft snapshot export to a configured storage backend. Not optional - startup fails if enabled without a snapshot target configured. Snapshot cadence defaults to hourly; retention configurable

## Rationale

Single-node Raft is consistent-by-trivia but has one disk = one failure domain. Without snapshot export, disk loss = total amnesia of an agent that knows your life. Enforced-at-startup prevents the easy mistake of forgetting to configure backup

