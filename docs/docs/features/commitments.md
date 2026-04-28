---
sidebar_position: 3
---

# Commitments

A **commitment** is the agent promising to do something later, then following through without prompting.

> **You:** when arsenal play next, ping me an hour before kickoff
>
> **Bot:** Got it — I'll check the fixtures and ping you an hour before the next match.
>
> *(2 days later, no further prompt from you)*
>
> **Bot:** Arsenal vs Spurs at 17:30 BST today, kickoff in 60 minutes. (Stadium: Emirates.)

The agent isn't running a fixed cron job. It made a promise, scheduled the followup, and when it fired, ran a fresh agent turn with the original commitment as the prompt.

## Anatomy

A commitment record:

```protobuf
message AgentCommitment {
  string id           = 1;  // ULID
  string created_for  = 2;  // user_id who asked
  string created_by   = 3;  // user_id who scheduled (usually same)
  string channel      = 4;  // originating channel (telegram, rest, ...)
  string channel_id   = 5;  // chat_id, etc. — for routing reply
  string prompt       = 6;  // the agent's brief to itself when it fires
  string when         = 7;  // RFC3339 or duration
  string status       = 8;  // pending | firing | complete | cancelled
  google.protobuf.Timestamp created_at  = 9;
  google.protobuf.Timestamp scheduled_for = 10;
  google.protobuf.Timestamp fired_at    = 11;
  string user_timezone = 12;  // for display + parse
}
```

Stored in a Raft-replicated bucket; survives leader failover.

## Creating

The agent calls `commitment_create`:

```
commitment_create(
  prompt: "Check the next Arsenal fixture. If it's within 24h, notify(text=...) about kickoff time and venue.",
  when:   "tomorrow 09:00",          # naked wall-clock; interpreted in __user_timezone
)
```

**Time parsing:** `when` accepts:

| Form | Example |
|---|---|
| Duration | `"2h"`, `"30m"`, `"3 days"` |
| RFC3339 with offset | `"2026-04-29T09:00:00+01:00"` |
| Naked wall-clock | `"tomorrow 09:00"`, `"2026-04-29 09:00"` — interpreted in user TZ |

User TZ priority: explicit user pref > cluster default > UTC.

## Firing

When `scheduled_for` arrives:

1. Scheduler finds the pending commitment, marks it `firing`.
2. A fresh agent turn spawns with the commitment's `prompt` as the user message and `created_for` as the active user.
3. The agent runs as if the user had just typed the prompt — full tool access, full memory, full normal flow.
4. When the turn completes, status → `complete`. If the prompt told the agent to `notify(text="...")`, the notification dispatches to the originating channel (or broadcasts if the originator is offline).

The commitment carries its originating channel context, so a Telegram-originated commitment fires a Telegram reply, even if the user has multiple bound channels.

## Cancelling

> **You:** cancel my arsenal commitment
>
> **Bot:** Cancelled — won't ping you about Arsenal.

The agent calls `commitment_cancel(id="01HX...")`. The scheduler marks it `cancelled`; if it's already firing, it's allowed to complete (no rollback).

## Listing

> **You:** what commitments do I have?
>
> **Bot:**
>
> | When | What |
> |---|---|
> | 2026-04-29 09:00 BST (in 22h) | Check Arsenal fixture, notify if within 24h |
> | 2026-05-01 17:00 BST (in 3d) | Daily summary of unread emails |

The agent calls `commitment_list(active=true)`.

## Why commitments and not cron?

A cron task is a fixed schedule. A commitment is a **promise**: the agent decides at firing time what to do. If you said "ping me when arsenal play next" and arsenal don't play for 6 weeks, the agent doesn't know what to do today — it knows what to *check* today, and only notify if conditions are met.

This is also why the firing semantics route through a fresh agent turn: the agent has the full toolbox at fire time, including web search, calendar lookup, etc.

## Common patterns

```
"check at 9am tomorrow if there are any GitHub issues assigned to me; if so summarise"
"every Monday at 8am, run the weekly_summary skill and send me the output"
"in 2 hours, ask me how the meeting went"
```

The third one is interesting — the commitment's `prompt` is a question for the user. When it fires, the agent sends "how did the meeting go?" as a message; your reply enters the conversation as a normal turn.

## Reference

- `internal/compute/builtin_commitment.go` — create / cancel / list / get
- `internal/scheduler/scheduler.go` — fire-due loop
- `pkg/proto/lobslaw/v1/lobslaw.proto` — `AgentCommitment` message
- `internal/node/wire_compute.go` — `runCommitmentAsAgentTurn` glue
