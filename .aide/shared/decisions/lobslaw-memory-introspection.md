---
topic: lobslaw-memory-introspection
decision: "Four new LLM-accessible builtins: memory_recent (list recent writes by retention tier), memory_forget (cascade via Service.Forget, destructive, confirmation-gated via RiskIrreversible), memory_correct (supersede: writes new memory with corrects:<old_id> tag then forgets old — audit trail preserved), dream_recap (list vector records with SourceIds.len > 1 = REM consolidations, newest-first). All read-only tools are store-scan (safe on followers); forget/correct require Raft leader. Registered when Raft + store + memory service are available. Humanisation: narrative tools narrated in voice, enumerable tools as tables/bullets, debug_* raw."
date: 2026-04-24
---

# lobslaw-memory-introspection

**Decision:** Four new LLM-accessible builtins: memory_recent (list recent writes by retention tier), memory_forget (cascade via Service.Forget, destructive, confirmation-gated via RiskIrreversible), memory_correct (supersede: writes new memory with corrects:<old_id> tag then forgets old — audit trail preserved), dream_recap (list vector records with SourceIds.len > 1 = REM consolidations, newest-first). All read-only tools are store-scan (safe on followers); forget/correct require Raft leader. Registered when Raft + store + memory service are available. Humanisation: narrative tools narrated in voice, enumerable tools as tables/bullets, debug_* raw.

## Rationale

Infrastructure existed (Service.Forget + cascade semantics) but no LLM surface — bot could only promise to forget. Correct via new-write + forget-old keeps audit trail without a separate supersede schema. Dream recap via SourceIds > 1 heuristic avoids schema changes; consolidations are already the only vector records with multiple sources.

