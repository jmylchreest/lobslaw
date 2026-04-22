---
topic: lobslaw-audit-log
decision: "Append-only hash-chained audit log in the same Raft group as policy and scheduled tasks. Each entry: actor scope, tool/service invoked, argv or method, policy rule id that allowed/denied, result hash, timestamp, prev-entry hash. Head hash periodically anchored by writing to a configured storage backend"
date: 2026-04-22
---

# lobslaw-audit-log

**Decision:** Append-only hash-chained audit log in the same Raft group as policy and scheduled tasks. Each entry: actor scope, tool/service invoked, argv or method, policy rule id that allowed/denied, result hash, timestamp, prev-entry hash. Head hash periodically anchored by writing to a configured storage backend

## Rationale

For an agent that can do anything with the right permissions, post-incident investigation needs tamper-evident history. Hash chain is cheap (~1 line per entry) and the same Raft group means no separate consensus to manage. External anchoring catches cluster-internal tampering

