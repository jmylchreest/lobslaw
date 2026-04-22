---
topic: lobslaw-agent-loops
decision: "Three loops: message processing (event-driven), scheduled task loop (cron-based, tasks stored as scheduled-task activity in memory), dream/REM loop (episodic consolidation)"
date: 2026-04-22
---

# lobslaw-agent-loops

**Decision:** Three loops: message processing (event-driven), scheduled task loop (cron-based, tasks stored as scheduled-task activity in memory), dream/REM loop (episodic consolidation)

## Rationale

Scheduled tasks must survive cluster restarts so they live in MemoryNode Raft, not ephemeral config. Dream/REM is a distinct loop with its own schedule and write-only memory model

