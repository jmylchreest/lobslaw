---
sidebar_position: 6
---

# Scheduler

Cron-style task scheduling, raft-replicated, agent-runnable.

## Two flavours

| Type | When | Created by |
|---|---|---|
| **Scheduled task** | cron expression — repeating | `schedule_create` builtin |
| **Commitment** | one-shot at a specific time | `commitment_create` builtin |

The scheduler runs both. Scheduled tasks recur until cancelled; commitments run once.

## Anatomy

```protobuf
message ScheduledTask {
  string id              = 1;  // ULID
  string name            = 2;
  string created_for     = 3;
  string cron            = 4;  // standard 6-field cron (with seconds)
  string prompt          = 5;
  string status          = 6;  // active | paused | cancelled
  google.protobuf.Timestamp created_at  = 7;
  google.protobuf.Timestamp last_run    = 8;
  google.protobuf.Timestamp next_run    = 9;
  string user_timezone   = 10;
}
```

Cron expressions use [robfig/cron](https://github.com/robfig/cron) syntax. Examples:

| Expression | Means |
|---|---|
| `0 0 9 * * *` | daily at 09:00 |
| `0 0 */2 * * *` | every 2 hours, on the hour |
| `0 30 8 * * MON` | every Monday at 08:30 |
| `@daily` | shorthand for `0 0 0 * * *` |

## Anchoring `next_run`

A subtle bug we hit: `cron.Next(time.Now())` returns the *next* firing after now. So a task created at 09:30 with `0 0 9 * * *` (9am daily) returns *tomorrow* at 9am, not today at 9am you'd expect.

Fix: anchor at `created_at` rather than `time.Now()` when computing the first `next_run`. After the first fire, `last_run` is the anchor.

```go
func taskNextRun(task *ScheduledTask, now time.Time) time.Time {
    schedule, _ := cron.ParseStandard(task.Cron)
    anchor := task.LastRun.AsTime()
    if anchor.IsZero() {
        anchor = task.CreatedAt.AsTime()  // <-- not now!
    }
    return schedule.Next(anchor)
}
```

Now a task created at 09:30 with "9am daily" fires today (because Next(09:30 yesterday) = 09:00 today, then 09:00 tomorrow, etc.).

## Firing

The scheduler ticks every `[scheduler] tick_interval` (default 1 min), finds tasks/commitments whose `next_run` ≤ now, and fires them.

A fire spawns a fresh agent turn with the task's `prompt` as the user message and `created_for` as the active user. Same path as commitments — see [Commitments](/features/commitments).

## Storage

```toml
[scheduler]
storage = "raft"             # raft | local
tick_interval = "1m"
```

`raft` (default) replicates the full task list across the cluster. Survives leader failover; on failover, the new leader's scheduler picks up where the old left off (idempotent — `last_run` prevents double-fire).

`local` stores tasks in a node-local file; useful for single-node dev.

## Time zones

Cron expressions are interpreted in `user_timezone`:

- User's `[[user]]` pref takes precedence.
- Cluster default (`[scheduler] default_timezone`) next.
- UTC if neither is set.

So `0 0 9 * * *` for a user in `Europe/London` fires at 09:00 BST during DST and 09:00 GMT outside it.

## Pausing and cancelling

```
schedule_pause(id="01HX...")     # status → paused; ticker skips it
schedule_resume(id="01HX...")    # status → active; recalculates next_run
schedule_cancel(id="01HX...")    # status → cancelled; record retained for audit
```

## Common patterns

```
"every weekday at 8am, summarise what's in my unread email"
"every Sunday at 6pm, ask me what I want to do this week"
"every 30 minutes, check the deploy job and notify me if it failed"
```

Last one is interesting — the prompt is a check, the agent only notifies if conditions are met. No notification = scheduled work that ran silently.

## Reference

- `internal/scheduler/scheduler.go` — ticker, fire-due, taskNextRun
- `internal/compute/builtin_schedule.go` — agent-facing CRUD
- `pkg/proto/lobslaw/v1/lobslaw.proto` — `ScheduledTask` message
- `internal/node/wire_compute.go` — `runTaskAsAgentTurn` glue
