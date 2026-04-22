---
topic: lobslaw-soul-adjustment
decision: "User feedback adjusts emotive style via feedback_coefficient (0-1). User says 'don't be snarky' → sarcasm -= coefficient × delta, persisted to local SOUL.md, cooldown prevents oscillation. Max ±3 per dimension"
date: 2026-04-22
---

# lobslaw-soul-adjustment

**Decision:** User feedback adjusts emotive style via feedback_coefficient (0-1). User says 'don't be snarky' → sarcasm -= coefficient × delta, persisted to local SOUL.md, cooldown prevents oscillation. Max ±3 per dimension

## Rationale

Soul adapts to user preferences over time without requiring config file edits. Small, bounded adjustments prevent runaway drift

