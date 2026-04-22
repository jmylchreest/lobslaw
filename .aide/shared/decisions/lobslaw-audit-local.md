---
topic: lobslaw-audit-local
decision: "Local audit sink in addition to the Raft-backed audit log. Format: audit.jsonl (one JSON entry per line, hash-chained via prev_hash). Rotation via natefinch/lumberjack.v2 with chain-across-rotation (final hash of old file = first prev_hash of new file). Optional head-hash anchor to a configured storage backend on cadence. Dual-write in clusters (defence in depth against a compromised node censoring its own Raft audit entries); sole audit in single-node mode (Raft audit can be skipped entirely for simplicity). lobslaw audit verify --local walks rotated files and reports the first chain break"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-audit-local

**Decision:** Local audit sink in addition to the Raft-backed audit log. Format: audit.jsonl (one JSON entry per line, hash-chained via prev_hash). Rotation via natefinch/lumberjack.v2 with chain-across-rotation (final hash of old file = first prev_hash of new file). Optional head-hash anchor to a configured storage backend on cadence. Dual-write in clusters (defence in depth against a compromised node censoring its own Raft audit entries); sole audit in single-node mode (Raft audit can be skipped entirely for simplicity). lobslaw audit verify --local walks rotated files and reports the first chain break

## Rationale

Raft-backed audit is authoritative but has two gaps: single-node deployments don't benefit from Raft audit complexity, and clustered deployments have no cross-check for a compromised node's own log-write path. Local JSONL audit is grep-friendly, log-shipper-friendly, and tamper-evident via the same hash chain scheme. Same AuditEntry struct, same hash algorithm - two sinks, one code path

