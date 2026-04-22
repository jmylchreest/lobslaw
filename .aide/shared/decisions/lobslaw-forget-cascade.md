---
topic: lobslaw-forget-cascade
decision: "Forget is aggressive-sweep by design: when a forget-query matches source records, any consolidated record whose SourceIDs intersect the matched set is ALSO deleted — even if other sources of that consolidation survive. PLAN.md's original spec called for re-consolidation on partial matches (keep the consolidation, drop the forgotten sources from its SourceIDs, queue a re-dream). Rejected in favour of deletion because a summary that retains one fragment of forgotten content still leaks that content — the consolidation's text and embedding were computed from ALL sources, so forgetting one source and keeping the summary means the forgotten source's influence is still encoded in the surviving summary"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-forget-cascade

**Decision:** Forget is aggressive-sweep by design: when a forget-query matches source records, any consolidated record whose SourceIDs intersect the matched set is ALSO deleted — even if other sources of that consolidation survive. PLAN.md's original spec called for re-consolidation on partial matches (keep the consolidation, drop the forgotten sources from its SourceIDs, queue a re-dream). Rejected in favour of deletion because a summary that retains one fragment of forgotten content still leaks that content — the consolidation's text and embedding were computed from ALL sources, so forgetting one source and keeping the summary means the forgotten source's influence is still encoded in the surviving summary

## Rationale

Right-to-be-forgotten is a first-class MVP requirement for a personal AI assistant. Partial re-consolidation preserves the summary's embedding, which was derived from the forgotten source — that's a data-leak path. Aggressive sweep is the privacy-safe choice. Deleted consolidations can be rebuilt by the next dream run from surviving sources; there's no permanent information loss, only a rebuild cost that's paid lazily

