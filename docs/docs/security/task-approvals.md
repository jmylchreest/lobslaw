---
title: Task approvals
---

Background runners can pause work for approval without discarding what they have
already done. Approval belongs to that task: it does not authorise sibling or
child tasks, or change ordinary chat approvals.

This page documents the shared task approval API. A runner must integrate it;
the API alone does not convert existing jobs into resumable tasks. The bot/team
console is developed separately in PR #348.

## Approval choices

- **Once** approves the saved pending operation once.
- **Operation** approves that stable operation for the task's remaining lifetime.
- **Risk labels** approves the displayed read/write categories for this task.
  Commands may differ, but each is classified and checked against current policy.
- **Budget extension** adds a specific allowance while preserving consumption.
- **Deny** stops the waiting operation.

An approval cannot override an explicit policy denial. A category approval is
not a promise of additional filesystem or network restrictions.

## HTTP API

Every endpoint requires a valid bearer token, even if normal chat permits
anonymous access. The owner is derived from that token's identity. A request
cannot select another owner. Responses omit private continuations and execution
tokens; the backend retains them for an authorised runner.

| Method and path | Purpose |
| --- | --- |
| `GET /v1/task-approvals?limit=50&after=ID` | List the caller's tasks; follow `nextAfterId` for another page. |
| `GET /v1/task-approvals/ID` | Read task state, revision, operation and budget information. |
| `POST /v1/task-approvals/ID/decide` | Approve or deny the saved request. |
| `POST /v1/task-approvals/ID/cancel` | Stop further task execution. |
| `POST /v1/task-approvals/ID/recover` | Acknowledge possible duplication and return an uncertain checkpoint to waiting. |

Use the latest returned `revision` in each mutation. A `409` response means the
state changed or the operation is no longer valid; refresh instead of blindly
repeating the request. JSON responses follow protobuf JSON conventions; the
64-bit revision is represented as a string. Send its numeric value in a request.

Approve once:

```json
{"revision": 2, "choice": "once"}
```

Add five tool calls to a budget prompt:

```json
{"revision": 2, "choice": "budget_extension", "extra_budget": {"tool_calls": 5}}
```

`extra_budget` also accepts `spend_usd` and `egress_bytes`. An exhausted dimension
must receive a positive extra allowance. The new limit starts from the greater
of its old limit and consumption so far. Zero never means an unlimited extension.

Other choices are `operation`, `risk_labels` and `deny`. The backend decides
which are valid from the saved operation; the request cannot substitute a new
command. Cancel takes `{"revision": 2}`.

## Uncertain outcomes

If a node loses its execution claim, an action might have happened even though
its result was not saved. The task reports `OUTCOME_UNKNOWN` and will not run
again automatically. Inspect the external result before choosing recovery.

```json
{"revision": 4, "acknowledge_duplicate_risk": true}
```

Recovery requires another approval and can repeat effects from the interrupted
leg. Cancelling running work cannot undo an action already in progress.

Tasks expire within 24 hours. Each execution leg has a 15-minute deadline;
integrations must keep their legs within that bound. Approval waits and outcomes
are discoverable by listing tasks after a restart. This feature does not add a
new Telegram notification flow or a console page.
