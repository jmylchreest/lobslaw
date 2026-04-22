---
topic: lobslaw-retention-forget
decision: "Retention tag on every memory write: session|episodic|long-term. Memory.Forget({query, before}) cascades through dream/REM consolidation - consolidated records keep provenance of source records and are pruned when all sources are forgotten. Tool-output writes default to session; user-authored context defaults to episodic; explicit user facts default to long-term"
date: 2026-04-22
---

# lobslaw-retention-forget

**Decision:** Retention tag on every memory write: session|episodic|long-term. Memory.Forget({query, before}) cascades through dream/REM consolidation - consolidated records keep provenance of source records and are pruned when all sources are forgotten. Tool-output writes default to session; user-authored context defaults to episodic; explicit user facts default to long-term

## Rationale

Right-to-be-forgotten is a first-class need for a personal agent, not an afterthought. Cascade through consolidation prevents forgotten data resurfacing as a 'summary'. Session default for tool output keeps ephemeral data from accreting into long-term memory by accident

