---
topic: lobslaw-proactive
decision: Cron-based scheduler for periodic tasks; self-healer with failure detection/recovery/diagnosis; supervised restart with backoff
date: 2026-04-21
---

# lobslaw-proactive

**Decision:** Cron-based scheduler for periodic tasks; self-healer with failure detection/recovery/diagnosis; supervised restart with backoff

## Rationale

Proactive waking + self-healing are core requirements; supervised restart aligns with zeroclaws channel pattern

