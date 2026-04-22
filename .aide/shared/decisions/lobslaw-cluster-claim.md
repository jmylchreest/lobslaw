---
topic: lobslaw-cluster-claim
decision: Scheduled tasks and inbound channel updates use Raft-CAS claim semantics for cluster-wide singleton execution. ScheduledTaskRecord and ChannelUpdateRecord carry ClaimedBy+ClaimExpiresAt fields; first compute/gateway node to CAS the claim wins and the rest skip. Completion CASes the claim off. No separate leader election - reuses the existing Raft group
date: 2026-04-22
---

# lobslaw-cluster-claim

**Decision:** Scheduled tasks and inbound channel updates use Raft-CAS claim semantics for cluster-wide singleton execution. ScheduledTaskRecord and ChannelUpdateRecord carry ClaimedBy+ClaimExpiresAt fields; first compute/gateway node to CAS the claim wins and the rest skip. Completion CASes the claim off. No separate leader election - reuses the existing Raft group

## Rationale

Without claim semantics, N compute nodes fire the same cron task N times. Reusing Raft is simpler than adding leader election. CAS with expiry handles crash recovery: an unclaimed-after-expiry task is picked up by the next node. Symmetric treatment of scheduled tasks and inbound channel updates avoids duplicate-processing bugs in both cases

