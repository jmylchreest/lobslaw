# Projects and autonomous work

Projects keep a shared conversation, an owned bot roster, tasks, reusable routines
and event triggers together. Tasks run in the background and retain their status
across a node restart. Attention shows completed deliverables, failures, blockers
and requests for approval.

## Prerequisites

- Enable `compute-teams` explicitly on the backend. `--all` does not enable it.
- Use a backend with the Raft/store and compute functions, configured model
  providers, and existing bots owned by your signed-in account.
- Enable `ui-web` where you serve the console. A web-only node uses its configured
  ConsoleService backend for project APIs and streams.
- Configure browser execution separately to run browser routines. The browser
  runtime is off by default, and its tool permissions remain subject to policy
  and the selected bot's tool allowlist.

Restore mode does not execute project work. Unavailable infrastructure returns
503; it does not create a pretend successful execution.

## Create a project and discuss work

Select a name, project context, coordinator and bot roster. Every selected bot
must belong to your account and be enabled. An optional group must also be owned
by you. The coordinator must be in the roster.

Project messages go to the selected bot, or to the project's coordinator when
you leave selection empty. One bot responds to a message; the whole roster does
not independently repeat the same request. Conversation history retains tool
activity internally so follow-up questions can refer to what actually happened.

The API example below assumes `BASE` is your gateway URL and `TOKEN` is an
existing user bearer token:

```sh
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  "$BASE/v1/projects" -d '{
    "name":"Weekly research",
    "description":"Prepare a short research report",
    "context":"Cite sources and explain uncertainty.",
    "coordinator_bot_id":"researcher",
    "bot_ids":["researcher"]
  }'
```

Use the returned project ID in subsequent requests. IDs, ownership and execution
state are assigned by the server. Sending an `owner` in a request does not change
who owns a record.

## Delegate tasks

Create a task with a title, instructions, optional acceptance criteria and an
optional assignee from the project roster. Without an assignee, the coordinator
runs it. Tasks without dependencies become ready immediately. To create a
dependency, put an existing task ID from the same project in `depends_on`.

```sh
curl -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  "$BASE/v1/projects/PROJECT_ID/tasks" -d '{
    "title":"Find primary sources",
    "instructions":"Research the topic in our project context and cite sources.",
    "acceptance_criteria":["Include source links", "Separate evidence from inference"],
    "depends_on":[]
  }'
```

| Status | Meaning / next action |
|---|---|
| `planned` | Waiting for dependencies to finish successfully |
| `ready` | Eligible for a worker claim |
| `running` | A worker has durably claimed and started the attempt |
| `blocked` | Human work or reconciliation is needed; read the question |
| `needs_approval` | Approve the recorded operation using the task's current revision |
| `done` | The attempt completed successfully; read its result artifact |
| `failed` | Read the error, correct the cause, then explicitly retry |
| `cancelled` | Cancellation invalidated the claim; late results cannot complete it |

Two workers run at a time on the leader. Each attempt is bounded to two minutes,
24 tool calls, $1 of model spend and 16 MiB egress, with stricter bot caps taking
precedence. One explicit budget approval can add one more budget tranche.

After an interrupted running attempt expires, the task becomes blocked instead
of automatically repeating an action that may already have happened. Review its
transcript or external system before retrying. Cancellation stops local work and
rejects a late completion; it cannot undo an external action already performed.

Task actions use `PATCH /v1/tasks/TASK_ID` with a current `revision` and one of
`start`, `retry`, `cancel`, `answer`, `approve` or `complete_step`. `answer` accepts
an `answer` string for a blocked task. Status, claim and result fields cannot be
set directly. Task approvals use this route, **not** `/v1/prompts`.

## Review Attention and deliverables

`GET /v1/attention` returns only your projects' blockers, approvals, failures and
completed task deliverables. Configured browser adapters can add human-control
items. Each text artifact references `GET /v1/tasks/TASK_ID/result`, which checks
ownership again. Artifact URLs are authenticated API routes, not disk paths.

## Save and run a routine

A saved routine starts as a draft. Review its instructions and ordered steps,
then explicitly approve its current revision. Editing any definition field,
including its schedule, clears approval. Running an unapproved or disabled
routine is refused.

For an instruction-only routine:

```json
{
  "name": "Morning summary",
  "description": "Review the project and prepare a short status report",
  "instructions": "Summarize current progress and outstanding questions.",
  "steps": [],
  "schedule": "CRON_TZ=UTC 0 9 * * 1-5"
}
```

POST that body to `/v1/projects/PROJECT_ID/routines`. Approve with
`PATCH /v1/routines/ROUTINE_ID` and `{"revision":1,"action":"approve"}` using
the actual revision returned by the API. The existing scheduler creates a task
for each due occurrence. `POST /v1/routines/ROUTINE_ID/run` runs it on demand and
returns its durable task, not an optimistic completion response.

Browser recordings use `navigate`, `click`, `fill`, `press`, `wait` and `capture`
steps. Fills and navigations containing URL credentials, queries or fragments
are manual sensitive steps; their input is not stored in the routine. Complete
the step in the browser, return control, then use `complete_step` on the blocked
task. For a takeover without a manual step, return control and retry the task.
Routine approval does not override browser policy or bot tool restrictions.

Missing browser runtime, denied actions and browser errors produce visible
failed/blocked/approval states. They are never reported as completed browser work.

## Connect an event trigger

POST a trigger to `/v1/projects/PROJECT_ID/triggers`:

```json
{"name":"New delivery","routine_id":"APPROVED_ROUTINE_ID","enabled":true}
```

A trigger can alternatively supply `instructions` and an optional
`assignee_bot_id` as its task template. Deliver events with an authenticated
POST to `/v1/triggers/TRIGGER_ID/fire`:

```json
{"event_id":"source-delivery-123","payload":{"document":"example"}}
```

The response is `{"task":...,"duplicate":false}`. Repeating the same event ID
for the same trigger returns the original task with `duplicate:true`, including
after restart. Supply a stable delivery ID from the source; generating a new ID
for every retry deliberately describes a new event. Event payloads are treated
as untrusted data and do not grant tools, ownership or approval.

## API and recovery reference

| Route | Methods |
|---|---|
| `/v1/projects` | GET list, POST create |
| `/v1/projects/{id}` | GET, PATCH fields with revision |
| `/v1/projects/{id}/messages` | GET display messages, POST bounded SSE conversation |
| `/v1/projects/{id}/tasks` | GET, POST |
| `/v1/tasks/{id}` | GET, PATCH explicit action with revision |
| `/v1/tasks/{id}/result` | GET authenticated text deliverable |
| `/v1/attention` | GET |
| `/v1/projects/{id}/routines` | GET, POST draft |
| `/v1/routines/{id}` | PATCH `approve`, `disable` or `edit`, with revision |
| `/v1/routines/{id}/run` | POST |
| `/v1/projects/{id}/triggers` | GET, POST |
| `/v1/triggers/{id}/fire` | POST with `event_id` |

All routes require a user identity. Cookie-authenticated mutations also require
a same-origin request. Cross-owner/unowned records return 403, unknown records
404, stale revisions 409 and unavailable infrastructure 503. Reload a record
after a 409 before deciding whether to retry your edit.

Each project holds at most 256 tasks, routines, triggers and event receipts, and
its total record is bounded to 4 MiB. Receipts are retained so old deliveries
cannot replay; there is no automatic receipt pruning. Start a new project when
capacity is reached. Archive a project to prevent new work from dispatching.

Project state is included in encrypted Raft snapshots. The logical portable
archive command does not currently include workforce projects; use your existing
Raft snapshot backup/recovery path for this state.
