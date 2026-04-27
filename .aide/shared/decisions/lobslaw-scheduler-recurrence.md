---
topic: lobslaw-scheduler-recurrence
decision: "Scheduler tools (schedule_create / list / get / delete) exposed to the LLM. schedule_create accepts natural language ('every 5m', 'every 1h', 'daily 08:00', 'hourly') OR raw cron, normalised to cron at write time via normaliseToCron. Sub-minute intervals rejected (scheduler tick is minute-granular). Tasks created via builtin use handler_ref 'agent:turn' — same ref node registers for operator-defined tasks — so one scheduler.runTaskAsAgentTurn handler fires both paths. Params carry prompt + notify_on (always/match/never, default match). ScheduledTaskRecord proto already has all needed fields; no proto changes."
date: 2026-04-24
---

# lobslaw-scheduler-recurrence

**Decision:** Scheduler tools (schedule_create / list / get / delete) exposed to the LLM. schedule_create accepts natural language ('every 5m', 'every 1h', 'daily 08:00', 'hourly') OR raw cron, normalised to cron at write time via normaliseToCron. Sub-minute intervals rejected (scheduler tick is minute-granular). Tasks created via builtin use handler_ref 'agent:turn' — same ref node registers for operator-defined tasks — so one scheduler.runTaskAsAgentTurn handler fires both paths. Params carry prompt + notify_on (always/match/never, default match). ScheduledTaskRecord proto already has all needed fields; no proto changes.

## Rationale

User wants agent-callable recurring tasks ('check my mail every 5 minutes'). Extending PlanService/existing ScheduledTask infra over inventing a new service — one source of truth, one handler, less forking. Natural-language parsing is tiny (two regexes) and covers the common personal-assistant phrasing without pulling a cron-expression library.

