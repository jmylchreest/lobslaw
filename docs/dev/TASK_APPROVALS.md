# Durable task approvals

Task approvals are independent of conversation approvals. A runner saves a
checkpoint before reporting that a task is waiting, and claims that checkpoint
before resuming it. The main integration points are the typed
`TaskApprovalService`, `compute.PauseTask`, `compute.TaskRunner` and
`compute.WithTaskExecution`.

This feature supplies the shared mechanism for PR #348. It does not include
that PR's team registry, console or inbox runner, and those runners must adopt
this contract before their delegated approvals work.

## Approved architecture

John approved these choices during the review of #348:

- A grant belongs to its owner, actor and task. Children need their own approval;
  a parent reference is attribution, not inherited authority.
- If a runner loses its execution claim, report an uncertain outcome. Do not
  automatically repeat actions which may have already happened. Recovery needs
  explicit acknowledgement of possible duplicate effects and fresh approval.
- Include explicit, bounded budget extensions. Retain prior consumption and
  never use the conversation path's unlimited `Budget.Relax()` for a task.
- Use typed gRPC operations. Specialist context remains deliberately supplied
  task context; resumption does not introduce owner-memory access.

## Interfaces and flow

`TaskApprovalService` exposes separate Create, Pause, Get, List, Decide, Claim,
Finish, Cancel, Recover and CheckGrant RPCs. See the protobuf definitions for
request fields and states. All RPCs are peer-only: a gateway asserts an
already-authenticated owner's canonical principal, while a runner asserts its
actor. An operator certificate is not a peer credential and cannot impersonate
these assertions. Owner-facing HTTP never accepts an owner from the body.

1. The trusted runner calls `CreateTaskApproval` with the owner, actor and optional
   parent. The service generates a new task ID and initial execution token.
2. The runner attaches `turn.TaskScope` through `compute.WithTaskExecution`,
   using an authoritative `CheckGrantTaskApproval` backend. It must do this for
   the initial leg as well as resumed legs. Every child gets a new task scope.
3. On `NeedsConfirmation`, call `compute.PauseTask` with the response and request.
   It saves the prepared invocation, transcript, supplied context, counters,
   limits and pending operation before returning waiting metadata.
4. An owner lists or retrieves waiting tasks, then makes a revision-checked
   decision. Operation/risk grants and the decision commit in the same record.
   A decision cannot substitute new command arguments.
5. A worker calls `TaskRunner.Resume`. The runner resolves **current** claims,
   tools and budget policy, claims the checkpoint, decodes it and resumes the
   saved turn. It preserves hook-rewritten parameters and rechecks policy.
6. Another confirmation checkpoints again. Success commits the result and
   clears grants, continuation and execution token.

The worker which integrates this feature must schedule ready tasks; the service
is not a second general-purpose task scheduler. It must release its worker or
HTTP stream while waiting. Waiting and ready metadata remain discoverable after
restart, so a notification is not the only route to completing the task.

`RemoteTaskApprovals` adapts a generated gRPC client for `TaskRunner`. Memory and
policy nodes register the service; followers forward to the leader. Gateway-only
nodes use discovered memory/policy peers or seed nodes. Reads which determine
execution authority verify leadership rather than trusting a stale follower.
Writes are not automatically retried after an ambiguous transport failure.

## State and lifetime

```text
running -> waiting -> ready -> resuming -> completed
              |                   |
              +-> denied          +-> waiting
              +-> expired

lost running/resuming claim -> outcome_unknown
outcome_unknown --explicit recovery--> waiting --fresh approval--> ready
```

Task authority lasts at most 24 hours. Each execution leg lasts at most 15
minutes; a long-running integration must arrange bounded legs. There is no
heartbeat or implicit lease renewal in this version. Expiry is checked on every
read/decision/grant check, independently of a sweeper. A live external process
cannot be rolled back by cancellation; cancellation stops further authorisation
and reports uncertainty if an action may already have started.

A waiting/ready cancellation becomes cancelled. Cancelling running work reports
an uncertain outcome and removes its replayable checkpoint. Explicit recovery
is available for a lost claim with a retained checkpoint and a live task
lifetime. Recovery clears reusable grants and old prepared approvals; retry may
repeat effects from the previous leg, which is why acknowledgement is required.

## Budget extensions

For an exhausted dimension, the owner supplies a positive additional allowance.
The new limit is `max(previous limit, consumed amount) + extra`. Dimensions not
extended retain their existing limits. Negative values, NaN/infinity, overflow,
zero-only extensions and extensions leaving an exhausted dimension blocked are
rejected. A zero value in the **extra allowance** never removes a limit.

The response exposes the saved consumption, policy and approved limits so the
owner can see what they are extending. A newly tightened operator policy is
applied on resume and can require another approval; previous consumption never
resets. These are approval limits, not an exactly-once accounting protocol for
external providers.

## Grants and execution checks

Existing command normalisation remains strict. A variable command can use a
read/write-category grant for the task; unreadable, network, deletion and
privilege categories are not offered as one-tap reusable category approvals.
Every command is classified again. Explicit policy denials and hard safety
checks still take precedence.

Task execution never consults the conversation grant cache. Missing durable
state, cancelled/expired claims, changed actors or backend failure prevent
execution, including a call that ordinary policy would otherwise allow. Checks
run before tool preparation and again at the policy gate. Task grants do not
promise additional filesystem or network confinement: the existing sandbox
policy remains in force.

## Persistence and compatibility

`TaskApprovalRecord` lives in the encrypted `task_approvals` bucket. Mutations
use Raft `LOG_OP_CLAIM` with expected revision and execution token. Concurrent
approvals or resume attempts have one winner. A claim does not make an arbitrary
external side effect atomic with the result write, so an expired claim is never
recycled automatically.

The continuation codec is shared with ordinary gateway prompts, preserving
main's prepared-call metadata. Ordinary chat approval scope is unchanged.
Portable archives do not export task authority or checkpoints. Full cluster
snapshots retain them as operational state, with their original deadlines.
Records are retained; this version does not add background retention deletion.

The protobuf additions are backwards compatible at the wire level, but all
Raft voters must understand the new log payload before tasks are created.
Old binaries deliberately reject unsupported replicated payloads. Do not route
task continuations through an older runner that drops task identity or prepared
metadata. Coordinate schema numbers when rebasing #348 onto this change.

## Integration with #348

- Create a fresh scope for each delegated/inbox task, even for the same bot.
- Supply current owner authority and bot tool restrictions through the runner's
  authority resolver; do not restore historical claims as current authority.
- Replace the child-error/terminal-inbox-failure handling with `PauseTask` and a
  waiting state. Save the task ID alongside the existing inbox/delegation record.
- Schedule ready task IDs through the existing worker mechanism. Use the saved
  task revision; do not retry an uncertain claim as a fresh inbox attempt.
- Surface waiting, denied, expired and uncertain outcomes in the console. Use
  the owner API for decisions and explicit recovery; never expose claim tokens.
- Reconcile parent/child completion without lending grants between them. The
  parent can remain available for unrelated work while its child waits.

## Verification

Coverage includes real-Raft concurrent decisions and claims, lost-claim recovery,
expiry without sweep, child isolation, write failures, owner-only HTTP, peer-only
RPCs, the actual agent resuming a prepared call once, bounded extra tool calls,
and a live node shutdown/restart with mTLS RPC and saved continuation recovery.
The existing conversation/continuation and hook tests remain regression checks.
