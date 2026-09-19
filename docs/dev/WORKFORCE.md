# Workforce

## Authority and execution

The opt-in `compute-teams` service owns projects, a shared conversation, task DAGs,
routine definitions and event receipts. HTTP and ConsoleService use the same
owner checks; a machine certificate never supplies a human owner.

Each project is one bounded, encrypted Raft record with a revision CAS. Keeping
event receipts and their tasks in this aggregate makes creation atomic. Workers
persist a random claim token before dispatch and complete only against that
original token. Expired running work becomes blocked for reconciliation rather
than automatically repeating a potentially irreversible external action.

```mermaid
flowchart LR
  Browser --> Auth[HTTP authentication / ConsoleService claims]
  Auth --> Owner[Explicit project owner check]
  Owner --> CAS[Raft project revision CAS]
  CAS --> Queue[Durable ready tasks]
  Queue --> Worker[Bounded worker pool]
  Worker --> Runner[turn.Runner with owned bot and budget]
  Worker --> Computer[Configured browser step executor]
  Runner --> Result[Claim-fenced checkpoint / approval / result]
  Computer --> Result
  Result --> CAS
  CAS --> Attention[Owner-filtered Attention]
```

Interfaces: `workforce.Service.AuthorizeProject(ctx, principal, projectID)`;
browser step execution is an injected function over the frozen RoutineStep JSON
shape. The browser implementation belongs to `internal/computer`.

## Persistence boundary

`WorkforceRecord` is a schema-versioned protobuf envelope in `BucketWorkforce`.
Its `state_json` contains typed Go `State` records: the project, tasks, execution
checkpoints, routine definitions, triggers, messages and event receipts. This is
an encrypted Raft record, not a filesystem database. `LOG_OP_CLAIM` requires an
explicit revision, and the FSM assigns the next revision. No service writes the
store directly.

The project aggregate is deliberately bounded: 4 MiB serialized, at most 256
tasks, routines, triggers and event receipts each. Event receipts are not evicted:
eviction would make an old delivery executable again. Content and routine steps
have additional limits defined in `internal/workforce/types.go`. This trades
large-project throughput for one atomic transaction covering receipt + task and
dependency/claim transitions. Raft snapshots include the bucket. Logical portable
archives do not currently export workforce projects.

Projects have their own editable revision; each task, routine and trigger also
has an item revision. Internal aggregate revisions fence concurrent workers
without making every task update invalidate an otherwise unchanged project form.
Pure mutations retry aggregate contention; external actions never run inside a
retry closure.

## Worker lifecycle

The `workforce` node wiring stage uses `gateComputeTeams`, after compute/auth and
before the gateway. A node without Raft/store has no local workforce backend.
The leader runs two workers, polling once per second. Ordinary compute and
restore mode do not enable workers.

```mermaid
stateDiagram-v2
  [*] --> ready: no dependencies
  [*] --> planned: existing dependencies
  planned --> running: all dependencies done + claim CAS
  ready --> running: claim CAS
  planned --> blocked: dependency failed or cancelled
  running --> done: successful response + original claim token
  running --> needs_approval: exact action/resource + continuation
  needs_approval --> ready: owner approves current task revision
  running --> blocked: human step / takeover / expired claim
  running --> failed: execution error or timeout
  failed --> ready: explicit retry
  blocked --> ready: answer / retry / complete_step
  running --> cancelled: owner cancellation invalidates claim
```

Dependencies must refer to existing tasks in the same project, and cannot be
edited by task actions. A newly minted task cannot be an ancestor of an existing
one, so this creation rule makes cycles impossible. Ready tasks are rechecked
against their dependencies before every dispatch.

Each attempt has a two-minute context deadline and an independently enforced
budget of 24 tool calls, $1 of model spend and 16 MiB egress, subject to stricter
bot limits. One explicit budget approval may add a second budget tranche.
Continuations preserve spent counters; approval never resets spend. The lease
expires after the attempt deadline plus 30 seconds. Expiry becomes a visible
blocker rather than automatically repeating an external effect whose outcome is
unknown. Exactly-once external effects are not promised across a process crash.

The worker rechecks the bot's owner, enabled flag and project roster at dispatch.
`turn.Runner` receives the bot principal, the original human claims, task ID,
budget and context. `compute.Adapt` resolves the bot profile and enforces its tool
filter. A timeout, cancellation or `NeedsConfirmation` response cannot complete
a task. Cancellation closes the local context and invalidates the durable claim;
late results are rejected by the original claim token. Each active worker also
observes its replicated claim once per second, so cancellation through another
backend closes its context rather than waiting for the full attempt timeout.

Full agent/tool transcripts use `memory.SessionService` under
`workforce:<task-id>`. Project conversation tasks serialize against pending
conversation tasks and load the previous assistant task's full transcript.
`ProjectMessage` is the display view, not a replacement for the tool transcript.
Project context is delimited as untrusted context.

## Approval and routine execution

A routine is created as a draft. Approval records the human owner and a digest
of its name, description, instructions, schedule and ordered steps. Editing or
disabling clears approval. A run snapshots that approved definition; dispatch
and each browser step reject a revoked or changed definition. These records do
not install executable skills or grant additional tool privileges.

Task resource approvals persist the paused transcript, budget, action and
resource inside the same task aggregate. `PATCH /v1/tasks/{id}` with action
`approve` resumes using the existing one-shot `turn.WithTurnApproval` mechanism.
The callback cannot choose a different operation. The task's `prompt_id` is a
correlation identifier, not a `/v1/prompts` registry entry.

```mermaid
sequenceDiagram
  participant Human
  participant API
  participant Raft
  participant Worker
  participant Policy
  participant Computer
  Human->>API: Save draft, then approve revision
  API->>Raft: Definition digest + approving owner
  Human->>API: Run routine
  API->>Raft: Task + approved definition snapshot
  Worker->>Raft: Claim original token
  loop From durable checkpoint
    Worker->>Raft: Recheck definition + token
    Worker->>Policy: tool:exec / browser_ACTION, bot allowlist
    alt Confirmation required
      Worker->>Raft: needs_approval + exact resource
      Human->>API: Approve task revision
      API->>Raft: Ready, one-shot operation grant
    else Manual step or human takeover
      Worker->>Raft: blocked, preserve checkpoint
      Human->>Computer: Complete manual work and release control
      Human->>API: complete_step or retry
    else Allowed
      Worker->>Computer: ExecuteStep
      Worker->>Raft: Advance checkpoint after success
    end
  end
  Worker->>Raft: Done only after all steps succeed
```

All recorded fills are manual sensitive steps. Sensitive steps cannot carry
selector, URL or value fields. Automated navigation rejects URL userinfo,
queries and fragments, which may contain credentials; those navigations are
manual steps instead. Browser availability never falls back to a permissive
tool. A missing executor is an explicit failed task.

## Browser adapter contract

`SetStepExecutor(func(context.Context, string, string, RoutineStep) error)` is a
boot-time hook. The strings are the verified human owner and project ID. The
computer adapter converts the frozen JSON fields, executes through the configured
runtime, and maps takeover/manual errors to `workforce.ErrBlocked`.
`AuthorizeStep` remains separate and evaluates `tool:exec` on
`browser_<action>` before calling the adapter; routine approval is not policy
approval. The worker also enforces the bot's explicit tool allowlist here.

`SetAttentionSource` accepts owned computer takeover items. Workforce rechecks
each returned item's project owner before including it. The computer authorizer
adapts `AuthorizeProject`; its HTTP layer should translate `ErrNotFound` to 404,
while owner mismatch remains 403. See the computer workstream's
`internal/computer` service for runtime configuration and process isolation.

## Scheduling and event dedupe

An approved routine with `schedule` creates an existing `ScheduledTaskRecord`
with handler `workforce:routine`, captured user claims and the approved digest.
The scheduler's persisted `NextRun` identifies the occurrence. Its handler calls
`ScheduledRun`, atomically writing the occurrence receipt and workforce task.
Scheduler claim replay therefore returns the same occurrence without a second
task. Revoked/edited routines fail the digest check, including old schedule
entries that have not yet been reconciled.

Authenticated trigger delivery uses `(trigger_id, event_id)` in the same durable
receipt map. The event payload is delimited as untrusted data. A trigger's
explicit task template or approved routine determines the work; payload fields
cannot supply an owner, principal, bot permission or approval.

## HTTP and remote console

`internal/gateway/rest_workforce.go` hosts the routes documented in
[the operator guide](../user/WORKFORCE.md). Every data route authenticates and
checks unsafe cookie requests for same-origin CSRF. The same handlers are on
the ConsoleService forwarding allowlist, so a web-only node supplies validated
human claims to its backend. Node mTLS is transport authentication only.
Project chat emits `start`, `reply`, `needs_confirmation` or `error` SSE events;
the durable task survives a disconnected browser. Login revocation cancels its
stream subscription.

Task artifacts currently contain a text deliverable reference served by the
owner-authorized `/v1/tasks/{id}/result` route. The API never turns a caller's
filesystem path into an artifact. Attention is derived from owned task states;
it is not a separate mutable inbox that can drift from the work.

## Verification

Tests cover real Raft shutdown/reopen and stale CAS; durable event and schedule
dedupe; cross-owner reads/mutations; dependency resolution; real
`compute.Agent` dispatch through `turn.Runner`; cancellation fencing; manual
checkpoints; definition revocation; browser policy and bot filters; secret-free
routine definitions; actual scheduler dispatch; and remote ConsoleService SSE.
Race tests cover workforce, gateway, node and memory packages. No new external
dependencies were added.
