---
topic: lobslaw-commitments
decision: "AgentCommitment record alongside ScheduledTaskRecord. Commitments are one-shot, user-originated ('check back on Y in 2h', 'remind me Tuesday'); ScheduledTasks are recurring operator-defined cron jobs. PlanService.GetPlan(window) returns upcoming commitments + scheduled tasks + in-flight long-running work + unresolved check-back threads. Built-in 'agenda' skill renders a plan through the soul voice for /plan-today style queries"
date: 2026-04-22
---

# lobslaw-commitments

**Decision:** AgentCommitment record alongside ScheduledTaskRecord. Commitments are one-shot, user-originated ('check back on Y in 2h', 'remind me Tuesday'); ScheduledTasks are recurring operator-defined cron jobs. PlanService.GetPlan(window) returns upcoming commitments + scheduled tasks + in-flight long-running work + unresolved check-back threads. Built-in 'agenda' skill renders a plan through the soul voice for /plan-today style queries

## Rationale

User explicitly asked for a 'what's your plan today?' surface. Cron is wrong for one-shot deferrals. Separating Commitment from ScheduledTask keeps each simple. PlanService as the single aggregation point means channels and skills don't duplicate query logic

