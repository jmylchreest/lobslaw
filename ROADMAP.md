# Roadmap — proposed work

Ordered, designed work that is **not yet built**. Distinct from the other planning docs:

| Doc | Holds |
|---|---|
| [PLAN.md](PLAN.md) | The phase plan — what shipped, in order, with scope notes |
| [DESIGN.md](DESIGN.md) | Design of things that exist |
| [DEFERRED.md](DEFERRED.md) | Conscious "not now" items with revisit triggers |
| **ROADMAP.md** (this) | Proposed next work: problem → design → acceptance |

Items graduate out of here: into `PLAN.md` as a phase when scheduled, into `DEFERRED.md` if
consciously dropped, or deleted once shipped (git history is authoritative).

## Basis

Derived from a feature/security/scalability comparison against
[NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent) and
[nullclaw/nullclaw](https://github.com/nullclaw/nullclaw) (August 2026). Where a design borrows a
concept or config vocabulary from either, it is cited inline — config familiarity is a feature, and
both projects have solved problems lobslaw hasn't reached yet.

R0–R12 are drawn from published docs. R13–R17 are drawn from hermes-agent's **source**, because the
load-bearing details — trigger cadences, cache-replay policy, the provenance mechanism, and the
failure modes they've already paid for — are not in its documentation. Cited by file and symbol so
the claims are checkable.

The comparison's one-line conclusion, which sets the ordering below:

> lobslaw's distributed core is more serious than either reference, but almost all conversational
> state lives in per-process maps at the edge. Fixing that one class of problem closes most of the
> interaction, consistency, recall *and* scalability gaps at once.

---

## Order of work

| # | Item | Priority | Effort | Blocks |
|---|---|---|---|---|
| **R0** | [Leader-forwarding write path](#r0--leader-forwarding-write-path) | 🔴 P0 | S | R1, R2, R3 |
| **R1** | [Session layer](#r1--session-layer) | 🔴 P0 | L | R2, R3, R4 |
| **R2** | [Durable, cluster-wide confirmations](#r2--durable-cluster-wide-confirmations) | 🔴 P0 | M | — |
| **R3** | [Turn serialisation + inbound queue](#r3--turn-serialisation--inbound-queue) | 🔴 P0 | M | — |
| **R4** | [Policy engine fails closed](#r4--policy-engine-fails-closed) | 🔴 P0 | XS | — |
| **R5** | [One trust contract + ingest scanning](#r5--one-trust-contract--ingest-scanning) | 🔴 P0 | M | — |
| **R6** | [Retrieval mechanics](#r6--retrieval-mechanics) — *[Retrieval](#retrieval--r6-r20-r21)* | 🟠 P1 | L | — |
| **R20** | [Vector scan cost](#r20--vector-scan-cost) — *[Retrieval](#retrieval--r6-r20-r21)* | 🟠 P1 | M | — |
| **R21** | [Embedding outbox](#r21--embedding-outbox) — *[Retrieval](#retrieval--r6-r20-r21)* | 🟠 P1 | S | — |
| **R7** | [Principal identity](#r7--principal-identity) | 🟠 P1 | M | — |
| **R8** | [Unified provider selection + fallthrough](#r8--unified-provider-selection--fallthrough) | 🟠 P1 | M | — |
| **R9** | [Hardline floor + protected paths](#r9--hardline-floor--protected-paths) | 🟠 P1 | S | — |
| **R10** | [Channel-agnostic Responder](#r10--channel-agnostic-responder) | 🟡 P2 | M | R11 |
| **R11** | [Channel breadth](#r11--channel-breadth) | 🟡 P2 | L | — |
| **R12** | [Memory transparency](#r12--memory-transparency) | 🟡 P2 | M | — |
| **R13** | [Progressive skill disclosure](#r13--progressive-skill-disclosure) | 🟠 P1 | M | R15, R16 |
| **R14** | [Pinned tier-0 memory](#r14--pinned-tier-0-memory) | 🟠 P1 | S | — |
| **R15** | [Self-taught store](#r15--self-taught-store) | 🟠 P1 | M | R16, R17 |
| **R16** | [Post-turn review fork](#r16--post-turn-review-fork) | 🟡 P2 | M | R17 |
| **R17** | [Self-taught lifecycle (curator)](#r17--self-taught-lifecycle-curator) | 🟡 P2 | M | — |
| **R18** | [Skills in the cluster store](#r18--skills-in-the-cluster-store) | 🟠 P1 | L | R15, R17 |
| **R19** | [Sign and pin the skill handler](#r19--sign-and-pin-the-skill-handler) | 🔴 P0 | S | — |

**R19 is exploitable today** and depends on nothing. It belongs with R4 in the
"land independently, first" set.

R13–R17 are the self-learning group, derived from reading hermes-agent's implementation
(`agent/background_review.py`, `agent/curator.py`, `tools/memory_tool.py`,
`tools/skill_provenance.py`, `tools/skill_usage.py`). R13 lands first — it is independent of the
rest and pays off at current scale, and it is what makes a growing skill library affordable.
R15 is the foundation for R16/R17: nothing self-taught is shippable until the store exists.

**R18 changes where skills live at all, and precedes R15.** Sequence for this group:
R13 → R18 → R14 → R15 → R16 → R17. R13 and R14 are independent of R18 and can land in any order
alongside it.

### Status drift

Written before Phase-12-era work landed. Re-verified against the code on
2026-08-15 — the table below is what the tree actually does, not what the
sections further down propose:

| # | State |
|---|---|
| R1 | **Largely landed** — `BucketSessions` + `BucketSessionMessages`, `internal/memory/session.go`, `internal/gateway/conversation.go`, `compute.ContextBudget`, rolling summary, `session_search`/`session_list`/`session_read`. Note the shipped design keeps messages in their own bucket rather than inline on the session record as proposed below |
| R4 | **Done.** `Engine.Evaluate` returns `EffectDeny`/"no rule matched (default-deny)" at the bottom, denies nil claims, and a rule whose conditions cannot be evaluated is skipped only when it would have allowed — deny and require_confirmation apply anyway |
| R19 | **Done for the handler.** `signing.go` + `ParseWithPolicy` enforce detached ed25519 signatures over `manifest.yaml`; the manifest pins `handler_sha256`, which is what makes the signature cover executable content, and the invoker re-hashes before exec. Signed manifests that pin nothing are rejected. Still open: only the handler is pinned, so a skill reading adjacent data files is unprotected, and there is no grace flag for migrating an existing signed corpus (nothing is deployed, so none exists) |
| R6 | **Partial** — `builtin_memory.go` does tokenised BM25-ish substring matching. The Raft-replicated inverted index, hybrid fusion and temporal decay are not in |
| R7 | **Partial** — see the status note on the section itself |
| R0 | **Done.** `RaftNode.ApplyOrForward` + `NodeService.Propose`. Sessions, prefs, credentials, soul tune, channel state and memory writes forward from a follower to the leader; Dream, session pruning and the scheduler stay leader-gated singletons, and `Forget` stays leader-only on purpose. See the section for the two deviations from the design below |
| R2, R3 | Not started. `require_confirmation` exists as a policy effect and an in-process `ErrRequireConfirm` with no durable record; turns are dispatched straight into `go func()` at `gateway/conversation.go:178` |
| R5 | Partial — `internal/soul/trust.go` and skill signing exist; the single trust contract and ingest scanning do not |

R5 is P0 on security grounds and independent of everything else. With R0 landed,
R2 and R3 are unblocked and are the remaining P0s.

---

## R0 — Leader-forwarding write path

**Blocks R1, R2, R3.** Do this first; it is small and everything else assumes it.

### Problem

Every Raft-backed write service refuses on a follower and pushes the problem to the caller:

```go
// internal/memory/user_prefs.go:93 — and the same shape in
// channel_state.go:96, soul_tune.go:80, credentials.go:149/181,
// service.go:225/257/353
return fmt.Errorf("user_prefs: not the raft leader; current leader is %s", s.raft.LeaderAddress())
```

There is **no forwarding anywhere in the tree.** That's tolerable for the current callers — Dream,
the scheduler and the channel poll loops are all singleton-gated to the leader by design. It is
fatal for everything in this roadmap: sessions, confirmations and turn leases are written by
*whichever gateway node the user's message landed on*, which is uncorrelated with Raft leadership.

Today a two-node cluster where the gateway is not the leader cannot persist a single conversation
turn. This is the concrete root cause of "the cluster is real but the edge is per-process".

### Proposal

Add a forwarding wrapper at the gRPC boundary, not inside each service. The service-level
leader checks stay exactly as they are — they become the assertion, not the user-facing error.

```go
// internal/node/leaderfwd.go
//
// LeaderForwarder turns a local write attempt into an RPC against
// the current leader when this node isn't it. Sits between the
// service and its callers so each service keeps its existing
// leader-only invariant unchanged: a service that returns
// ErrNotLeader is correct, and the forwarder is what makes that
// invisible to callers that don't care.
//
// Deliberately NOT a retry loop over Apply: forwarding is one hop
// to a known address over the cluster's existing mTLS connection.
// Leadership churn mid-forward surfaces as ErrNotLeader from the
// far side, which the caller retries — bounded, observable, and
// it can't mask a partition as latency.
type LeaderForwarder struct {
    raft  *memory.RaftNode
    peers PeerDialer // existing mTLS client pool
}

func (f *LeaderForwarder) Do(ctx context.Context, fn LocalWrite, rpc RemoteWrite) error
```

Rules:

- **Read local, write forwarded.** Reads already bypass Raft (`Store.Get`) and stay that way.
- **Bounded hops.** A forwarded write never forwards again — the far side either applies or returns
  `ErrNotLeader`. One hop, no cycles.
- **`ErrNoLeader` is distinct from `ErrNotLeader`.** During an election there is no address to
  forward to; callers need to distinguish "wait and retry" from "wrong node".
- **Single-node deployments never forward** — `IsLeader()` is always true, so the path is free.

Every new write in R1/R2/R3 goes through this. Retro-fitting the existing services is optional and
can happen opportunistically.

### Acceptance

- [x] A three-node cluster where the gateway node is a follower persists sessions, prompts and
      leases with no operator action. *Sessions, user prefs, credentials, soul tune, channel state
      and memory Store/EpisodicAdd forward. Prompts and leases are R2/R3 and do not exist yet;
      they inherit the path.*
- [x] Killing the leader mid-turn surfaces a retryable error, not a lost message.
- [x] `ErrNoLeader` and `ErrNotLeader` are separately testable and separately logged.

**Shipped 2026-08-15**, with two deviations from the proposal above:

- **Not at the gRPC boundary.** The writes this exists for — sessions above all — have no gRPC
  surface at all; they are in-process Go calls from the agent to `memory.SessionService`. A wrapper
  at the gRPC boundary would not have seen them. Forwarding instead sits at the Raft handle, as
  `RaftNode.ApplyOrForward`, which every write path already funnels through: each one marshals a
  `LogEntry` and calls `Apply`.
- **One new RPC, `NodeService.Propose`,** carrying an already-marshalled `LogEntry`. It grants a
  peer no authority it lacked: the Raft transport shares the same gRPC server and the same cluster
  mTLS identity, and already accepts arbitrary log replication from any member, so the trust
  boundary is cluster membership rather than this method. It is leader-only and never forwards,
  which makes a cycle structurally impossible rather than merely bounded.

`Forget` is deliberately left leader-only: it scans for a matched set and then deletes it, and
forwarding each delete individually would run the scan against the follower's view while the
deletes landed on the leader.

---

## R1 — Session layer

Addresses review §3.1. **Blocks R2, R3, R4.**

### Problem

lobslaw has *memory*; it has no *sessions*. hermes draws the line explicitly, and it's the right
line: *"Memory is for critical facts always available. Session search is for 'did we discuss X last
week?' queries."*

| Channel | Conversation state today |
|---|---|
| REST | **None.** No `session_id`, no history hydration. Every `POST /v1/messages` is a cold turn. |
| Telegram | `internal/gateway/telegram_history.go:22` — 100 msgs, 30min TTL, `map[int64]*historyBucket` |
| Webhook | None |

The Telegram buffer's own doc comment states the consequence: *"Ephemeral by design: conversation
context lost on process restart."* In a cluster it is worse than ephemeral — it is **divergent**.
Two gateway nodes hold different views of the same chat, so which history the model sees depends on
which node Telegram's webhook happened to reach.

### Proposal

A first-class, Raft-replicated `Session`. One record per conversation, holding a bounded message
window plus a summary of everything that fell out of it.

**Why one record and not one record per message:** a turn is one Raft apply and a load is one
`Store.Get`. Per-message records would multiply applies by conversation length and turn history
loading into a range scan — for a bounded window we'd be paying scan cost to reconstruct something
that fits in a single value. The cost is rewriting the window on each append, which is a few KB.

```proto
// pkg/proto/lobslaw/v1 — new
message SessionRecord {
  string id             = 1;  // derived, see below
  string principal_id   = 2;  // R7; until then, Claims.UserID
  string channel        = 3;  // "telegram" | "rest" | "webhook"
  string channel_id     = 4;  // chat_id, API session, webhook source
  google.protobuf.Timestamp created_at     = 5;
  google.protobuf.Timestamp last_active_at = 6;
  repeated Message messages = 7;  // bounded window, newest last
  string  summary       = 8;  // compacted prefix (see Compaction)
  uint64  turn_count    = 9;
  Retention retention   = 10;
  repeated PendingInbound pending = 11;  // R3
  repeated SessionGrant   grants  = 12;  // R2 "approve for this session"
}
```

**Deterministic IDs.** `session_id = hex(sha256("v1:" + channel + ":" + channel_id))[:32]`.
Any node derives the same ID from the inbound message with no lookup and no allocation round-trip —
which is what makes a follower able to load a session before it knows whether it can write.
Version-prefixed so the scheme can change without colliding with existing records.

**New buckets** in `internal/memory/buckets.go`:

```go
BucketSessions     = "sessions"       // SessionRecord, keyed by session_id
BucketSessionIndex = "session_index"  // "<principal>:<last_active_unix>:<session_id>" -> nil
```

The index bucket exists so "list my recent sessions" (R12, and the `history` CLI in R6) is a bbolt
prefix scan in recency order rather than a full-bucket walk. Key ordering does the sorting.

**Service** — `internal/memory/session.go`, same shape as the neighbouring services:

```go
func (s *SessionService) Load(ctx, channel, channelID string) (*SessionRecord, error) // local read
func (s *SessionService) Append(ctx, id string, msgs ...*Message) error               // via R0
func (s *SessionService) Compact(ctx, id string, summariser Summarizer) error
func (s *SessionService) List(ctx, principal string, limit int) ([]*SessionRecord, error)
func (s *SessionService) Forget(ctx, id string) error
```

**Compaction, not truncation.** Today the buffer drops the oldest messages silently once it exceeds
100 — the start of a long conversation vanishes with no trace. Instead: when the window exceeds
`max_messages`, the overflow prefix goes to the existing `Summarizer` interface (already defined for
Dream, already has an LLM binding via `RoleSummariser`) and the result is folded into
`SessionRecord.summary`, which renders ahead of the window as short-term context. Compaction runs
off the hot path — the turn that trips the threshold returns first, compaction fires behind it, same
pattern as `maybeIngestTurn`.

**Retention.** Sessions are `RETENTION_SESSION`, so the existing `SessionPruner` (which already
hard-deletes session-retention records past `MaxAge`, default 24h) needs only to learn the new
bucket. Raise the default to something conversational — 30 days — and make it configurable;
24h is right for scratch records and wrong for "what did we decide last week".

**Channel wiring.** `chatHistory` becomes an interface with two implementations, so single-node dev
keeps a zero-dependency path:

```go
type SessionStore interface {
    Load(ctx context.Context, channel, channelID string) ([]compute.Message, string, error) // msgs, summary
    Append(ctx context.Context, channel, channelID string, msgs ...compute.Message) error
}
```

`memorySessionStore` (the current map, unchanged semantics) and `raftSessionStore`. Selected by
whether the node has the memory function wired — no new config knob.

**REST gains sessions.** `POST /v1/messages` accepts an optional `session_id` and always returns the
one it used, so a client can be stateless and still continuous:

```jsonc
// request
{ "message": "and what about the second one?", "session_id": "a3f1…" }
// response
{ "reply": "…", "session_id": "a3f1…", "turn_count": 7 }
```

Omitted `session_id` on REST means "new session" (returns a fresh one). A REST client that wants the
Telegram-style implicit continuity passes a stable `channel_id` of its own instead.

### Migration & compat

Additive. No existing record changes shape. The in-memory store stays the default until a node has
memory wired, so single-node behaviour is bit-identical to today apart from surviving restart.

### Acceptance

- [ ] Restart mid-conversation; the next Telegram message continues with full context.
- [ ] Two gateway nodes alternating on the same chat produce one coherent history.
- [ ] A REST client passing `session_id` gets continuity; one that omits it gets a fresh session.
- [ ] A conversation past `max_messages` retains a summary of the dropped prefix, and the model
      demonstrably still knows a fact stated only in the dropped region.

---

## R2 — Durable, cluster-wide confirmations

Addresses review §3.2.

### Problem

`PromptRegistry` and the Telegram `continuations` map are both in-memory and per-handler.
`docs/dev/GATEWAY.md` is honest about the consequences: approval after a restart tells the user to
resend, and node-A-sends-keyboard / node-B-receives-tap simply fails.

Worse, and independent of clustering: there is **no "approve for this session" and no "always"**.
Every confirmation is one-shot, so the user is re-prompted for the same operation forever. Both
references solved this; hermes offers `once / session / always / deny` and mines approval history
into suggested rules.

### Proposal

Move the registry into Raft and give approvals a scope.

```proto
message PromptRecord {
  string id          = 1;
  string turn_id     = 2;
  string session_id  = 3;   // R1
  string reason      = 4;
  string channel     = 5;
  string channel_id  = 6;   // where to deliver the resolution
  PromptDecision decision = 7;  // PENDING|APPROVED|DENIED|TIMED_OUT
  PromptScope    scope    = 8;  // ONCE|SESSION|ALWAYS
  string resolved_by = 9;       // principal
  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp expires_at = 11;
  Continuation continuation = 12;  // conversation + budget, to resume anywhere
  string action   = 13;  // policy action, for ALWAYS
  string resource = 14;  // policy resource, for ALWAYS
}
```

**Atomic resolve, for free.** The FSM already has the CAS primitive this needs —
`LOG_OP_CLAIM` / `applyClaim` with `ExpectedClaimer` (`internal/memory/fsm.go:191`), built for the
scheduler. `Resolve` becomes a CAS from `PENDING` to the new decision. First writer wins
*cluster-wide*, which is the same guarantee `TestPromptRegistryConcurrentResolveOnlyOneWinner`
already pins in-process — the test should survive the port with the registry swapped underneath it.

**Continuation must be serialisable.** This is the substantive work: the Telegram handler currently
stashes a live `[]compute.Message` and `*TurnBudget` in a Go map. Both need proto forms so a
different node can resume. Budget counters serialise cleanly (they're integers plus a relax flag);
the conversation slice is already `Message`, needed by R1 anyway.

**TTL by sweeper, not by timer.** `time.AfterFunc` is per-process, so a prompt created on a node
that then dies never times out. Replace with a leader-gated sweeper (`internal/singleton` leader
gate already provides the gating) that flips expired `PENDING` records. Slightly coarser than a
timer — bound the sweep interval at a few seconds so the user-visible behaviour is unchanged.

**Approval scope.** The interaction fix, and the part where lobslaw can do better than either
reference because policy rules are already replicated state rather than a line in a YAML file:

| Scope | Effect |
|---|---|
| `once` | Current behaviour — this turn only |
| `session` | A `SessionGrant` on the `SessionRecord` (R1), expiring with the session |
| `always` | Mints a `PolicyRule` in `BucketPolicyRules`: subject = resolving principal, action + resource from the prompt, effect allow, `created_by = "approval:<prompt_id>"` |

The `created_by` provenance matters — it makes `lobslaw policy ls --from-approvals` and a
one-command revoke possible, so "always" isn't a decision the user can never see again. Telegram
renders four buttons instead of two; REST takes `{"approve": true, "scope": "session"}`.

**Hardline interaction:** `always` can never grant anything the R9 floor denies. Assert this in a
test rather than trusting call ordering.

### Acceptance

- [ ] Approve on node B a prompt issued by node A.
- [ ] Approve after a full process restart; the turn resumes rather than asking the user to resend.
- [ ] Concurrent resolves cluster-wide: exactly one winner, everyone else `ErrPromptResolved`.
- [ ] `always` produces a visible, revocable policy rule.
- [ ] `always` cannot escalate past the hardline floor.

---

## R3 — Turn serialisation + inbound queue

Addresses review §3.3.

### Problem

Each Telegram webhook is handled on its own goroutine. The sequence is `Load` (returns a defensive
copy) → run the turn (seconds, with tool loops) → `Append`. Two messages arriving during one turn
both read the same prior history and both append. The result is interleaved, duplicated,
order-dependent history — and with R1 that corruption becomes durable instead of evaporating in
30 minutes.

There is also no answer to the ordinary human behaviour of sending three short messages in a row.

### Proposal

**Per-session lease** via the same `LOG_OP_CLAIM` CAS used in R2. Key: `session_id`. Value: holder
node ID + expiry. TTL matches the responsiveness hard timeout (90s, from
`telegram_responsiveness.go`) so a node that dies mid-turn releases within one bounded window rather
than wedging the conversation. Heartbeat while a turn runs long.

Single-node deployments take the same code path — the CAS is local, so there is one mechanism to
reason about rather than a fast path and a correct path that can disagree.

**Queue modes**, borrowing nullclaw's vocabulary directly (`agent.default_queue_mode` +
`messages.inbound.debounce_ms`) because config familiarity is worth more here than novelty:

| Mode | Behaviour |
|---|---|
| `serial` | Queue behind the in-flight turn, process in arrival order (default) |
| `latest` | Keep only the newest queued message; drop the rest with a note in the reply |
| `debounce` | Hold for `debounce_ms` (default 3000) and fold consecutive messages into one turn |
| `off` | Drop messages arriving during a turn (with a "still working" note) |

`debounce` is the one that matches how people actually type; `serial` is the safe default.

**Queued messages persist.** Pending inbound goes on `SessionRecord.pending` (R1), so a restart
mid-queue doesn't silently eat the user's message. This is why R3 depends on R1 rather than being a
standalone mutex.

**Slash-style commands bypass debounce** (nullclaw does this, and it's correct — a command is not a
sentence fragment).

**Responsiveness interaction:** debounce holds the typing indicator rather than starting and
stopping it per fragment, so the UX reads as "it's listening" instead of stuttering.

### Acceptance

- [ ] Three messages sent during one in-flight turn produce one coherent history in arrival order.
- [ ] `debounce` folds rapid-fire fragments into a single turn.
- [ ] A node killed mid-turn releases its lease within the TTL and another node picks up the queue.
- [ ] Queued messages survive a restart.

---

## R4 — Policy engine fails closed

Addresses review §4. **Smallest item here; land it independently of everything else.**

### Problem

`internal/policy/engine.go:111` — a condition evaluator that errors causes the rule to be **skipped**:

```go
if ok, err := e.conditionsHold(ctx, rule.Conditions); err != nil {
    e.logger.Warn("policy: condition evaluation error", "rule_id", rule.ID, "err", err)
    continue   // ← the rule might have been a DENY
}
```

`ARCHITECTURE.md`'s own sequence diagram flags it: *"log warn, SKIP rule (⚠ see TODO: fail-closed)"*.

Rules are walked in priority order and the first match returns. An evaluator error means *we do not
know whether this rule matched* — and a rule we can't evaluate may be the deny that mattered.
Skipping it is the one unsafe choice available.

### Proposal

```go
if ok, err := e.conditionsHold(ctx, rule.Conditions); err != nil {
    // Fail closed: we cannot determine whether this rule matched,
    // and an unevaluable rule may be the deny that mattered. Error
    // (not warn) — an evaluator that errors is a bug or an outage
    // in a dependency, and either way it is now denying traffic.
    e.logger.Error("policy: condition evaluation failed; denying",
        "rule_id", rule.ID, "action", action, "resource", resource, "err", err)
    return Decision{
        Effect: types.EffectDeny,
        RuleID: rule.ID,
        Reason: fmt.Sprintf("condition evaluation failed for rule %q", rule.ID),
    }, nil
}
```

Add `[policy] condition_error_mode = "deny" | "skip"`, default `"deny"`, so an operator with a
flaky custom evaluator has an escape hatch that is a deliberate, documented, logged-at-boot choice
rather than the silent default. Update the `ARCHITECTURE.md` diagram in the same commit (per
`lobslaw-documentation-diagrams`) and delete the TODO.

### Acceptance

- [ ] An erroring evaluator on a deny rule denies.
- [ ] An erroring evaluator on an allow rule denies (not "falls through to a later allow").
- [ ] `condition_error_mode = "skip"` restores the old behaviour and logs a warning at boot.

---

## R5 — One trust contract + ingest scanning

Addresses review §4.

### Problem — restated more precisely than in the review

Recall is **not** un-delimited, as I first said. `ContextEngine.Assemble` wraps each record in
`<relevant_context score=… when=…>` and prefixes an instruction that content inside is data, not
instructions. That's a reasonable defence and I was wrong to describe it as absent. The actual
problems are narrower and still worth fixing:

1. **Two delimiter contracts.** `pkg/promptgen` defines `TrustUntrusted` → `<untrusted source=…>`,
   and `BuildSafety` trains the model on *that* tag. `ContextEngine` emits a bespoke
   `<relevant_context>` tag the safety block never mentions, carrying its own inline restatement of
   the rule. Two implementations of one contract, free to drift — and the recall path is the one
   that isn't covered by the safety training.

   `promptgen`'s own doc comment shows this was anticipated: *"Default for tool output, memory
   recall, fetched content…"*, and `ContextCategory` already has a `CategoryLongTerm` for
   *"long-term (vector-recalled) reference"*, rendered as a "Recalled Memory" section. The API for
   this exists and is unused.

2. **No delimiter neutralisation, in system-prompt position.** `WrapContext` deliberately does not
   escape, and documents why: *"a user who includes `</untrusted>` in their message CAN close the
   block; the safety training on the model side treats this as attempted injection."* That trade is
   defensible where the wrapped block sits in a *user-turn* message. Recall does not sit there —
   `agent.go` appends it to `req.SystemPrompt`, the most privileged position in the request. A
   poisoned episodic record containing `</relevant_context>` followed by instruction-shaped text
   escapes into system-prompt scope, and the only thing standing in its way is model training
   against a tag name the safety block doesn't use.

   Reaching an episodic record is not hard: ingest stores the user message verbatim, and fetched
   web content can be summarised into memory.

3. **No ingest-time scanning.** hermes scans memory entries for injection patterns and invisible
   Unicode *specifically because* they land in system prompts, and scans SOUL.md / AGENTS.md too.
   lobslaw scans nothing on any of those paths.

### Proposal

**One contract.** `ContextEngine` stops rendering strings and returns blocks:

```go
type ContextAssembly struct {
    Blocks    []promptgen.ContextBlock  // Category: long-term, Trust: TrustUntrusted
    RecallIDs []string
}
```

The agent calls `promptgen.WrapContext`. The per-record `score` / `when` metadata moves onto the
`Source` field (`memory:recall score=0.91 when=…`), which `WrapContext` already renders as an
attribute. Net effect: recall is covered by `BuildSafety`, and the safety contract has exactly one
implementation.

**Move recall out of the system prompt.** Recall becomes a leading `role=user` context message
rather than a system-prompt suffix. This is the structural fix, and it makes the existing
no-escape decision correct again by putting untrusted content back in the position that decision
reasoned about. It also stops recall from invalidating the prompt prefix cache on every turn, which
is a free latency and cost win — hermes freezes its memory snapshot at session start for exactly
this reason.

**Neutralise anyway, for anything system-bound.** Belt and braces for whatever still needs to be
system-position:

```go
// promptgen.NeutraliseCloseTags inserts a zero-width space into any
// sequence that would close one of our delimiters. Reversible for a
// human reader, inert as a tag. Applied ONLY on the system-prompt
// path — the documented no-escape trade above holds for user-turn
// position and we don't want two behaviours for the same tag.
func NeutraliseCloseTags(s string) string
```

**Scan on ingest** — new `internal/promptguard`:

| Detector | Catches |
|---|---|
| Invisible Unicode | zero-width, bidi overrides, tag block (U+E0000–U+E007F), soft hyphen runs |
| Delimiter fragments | `</untrusted`, `</relevant_context`, `<\|im_start\|>`-style control tokens |
| Instruction shapes | "ignore previous/prior instructions", "you are now", "new system prompt" |
| Exfil shapes | credential paths adjacent to a URL; `curl`/`wget` piping a secret-shaped token |

Applied at four sites: episodic ingest, memory builtin writes, SOUL/fragment load, skill manifest
load.

**Quarantine, don't drop.** A detection sets `metadata["promptguard"] = "<detector>"` and the record
is stored. Recall skips quarantined records by default; `lobslaw memory ls --quarantined` shows
them. Dropping silently would make a false positive undebuggable, and the record is often the
evidence you want. hermes hard-blocks context files with a visible
`[BLOCKED: … contained potential prompt injection]` marker — same spirit, kept inspectable.

**Also worth doing here** (small, same area): route tool errors and MCP output through a redactor
for `ghp_`, `sk-`, `xoxb-`, bearer tokens and PEM blocks before they reach logs or the model.
`pkg/config/secret.go` already has the `Secret` type; this is the log/error-path counterpart.

### Acceptance

- [ ] Recall renders inside `<untrusted>` with a `memory:recall` source, in user position.
- [ ] A record containing `</untrusted>` cannot escape its block on any path.
- [ ] A record containing zero-width characters or an "ignore previous instructions" phrase is
      quarantined at ingest and excluded from recall.
- [ ] Prompt prefix stays stable across turns in a session (cache-hit assertion).
- [ ] `BuildSafety` names every delimiter that can appear in a request. Test enumerates both sides.

---

# Retrieval — R6, R20, R21

One body of work, three landable pieces. Addresses review §3.4.

**Rewritten 2026-08-15** after reading hermes-agent's and nullclaw's retrieval source, re-reading
what lobslaw actually ships, and — for the first time — **measuring it**.

## Ground rule: Raft only

Per `lobslaw-retrieval-raft-only`: every index and every derived structure in this section lives in
the Raft-replicated store. **No external vector database, no pluggable `VectorStore` vtable, no
sidecar.** nullclaw offers qdrant / pgvector / lancedb behind a vtable; that is explicitly *not* the
direction here — it would be a second unreplicated source of truth, an egress and
data-classification event, and a sidecar this project just finished removing.

Anything that cannot be expressed as Raft-resident derived state is out of scope for retrieval.

## Measured baseline

`internal/memory/search_bench_test.go` (new — first benchmark this path has ever had). D=1536,
sealed bbolt store, `go test ./internal/memory -run '^$' -bench Vector -benchmem`:

| N | ns/op | per record | B/op |
|---|---|---|---|
| 100 | 1.33 ms | 13.3 µs | 1.4 MB |
| 1,000 | 13.9 ms | 13.9 µs | 13.8 MB |
| 10,000 | **125.5 ms** | 12.6 µs | **138.8 MB** |

Exactly linear, ~12.5 µs and ~13.9 KB **per record per query** — and `ContextEngine` calls this
passively on every turn. The comment in `search.go:47` says *"Fine for personal scale (< ~100k
records)"*. Extrapolated, 100k records is **~1.25 s and ~1.4 GB of allocation per user message.**
That claim is wrong by a wide margin and should be deleted along with the fix.

### Where the time actually goes (N=10,000, D=1536)

| Layer | ns/op | share | B/op |
|---|---|---|---|
| decrypt only — `ForEach` with a no-op body | 88.8 ms | **~71%** | 68.1 MB |
| \+ `proto.Unmarshal` | 113.6 ms | ~91% | 138.1 MB |
| cosine arithmetic alone, no I/O | 19.6 ms | ~16% | 0 |
| └ of which the redundant `norm()` | 3.3 ms | ~3% | 0 |
| `sort.Slice` over all candidates vs top-3 | 1.3 ms | ~1% | — |

**The cost is not the cosine.** The arithmetic the algorithm actually needs is its *smallest*
component. Every query secretbox-decrypts and proto-decodes the entire corpus, including
`VectorRecord.Text`, which scoring never reads.

Dimension scaling confirms it — at N=2,000, 4× the width costs only 2.3× the time
(D=384 → 12.5 ms, D=768 → 17.9 ms, D=1536 → 28.3 ms), so roughly half the per-record cost is
dimension-independent overhead.

> **This changes the ANN design.** A band prefilter only helps if the band lookup **avoids the
> decrypt**. Signatures stored inside the sealed `VectorRecord` would require decrypting everything
> to read them — zero benefit for real added complexity. See R20.

The codebase already solved this exact problem once, for transcripts —
`Store.ForEachPrefix`: *"a full ForEach would decrypt every message in the cluster to read one
conversation. The bbolt cursor seeks straight to the range."* Vector search needs the same move.

## R6 — Retrieval mechanics

### What the first draft got wrong

Three corrections, because they change the work:

1. **The architecture is not the problem.** lobslaw's layering — verbatim transcript
   (`BucketSessionMessages`) / distilled + embedded + consolidated (`EpisodicRecord` + `VectorRecord`
   + Dream) / pinned curated (R14) — is better than either reference. hermes has a raw log plus
   hand-curated markdown files and *nothing between them*, so importance, retention, consolidation
   and ownership have nowhere to live. Do not restructure. Fix the mechanics.
2. **There is already a de-facto hybrid.** `memory_search` runs semantic first and augments with
   tokenised substring when semantic under-delivers, reporting which ran
   (`semantic` / `semantic+substring` / `tokenised-substring`). What is missing is a *fused ranking*,
   not a second strategy.
3. **The trajectory is not lost — it is linked.** `EpisodicRecord.session_ref`
   (`"<channel>:<channel_id>:<seq>"`) already points a recall hit at its transcript position, with
   the right advisory caveat. So the earlier "fatten `EpisodicTurn` with tool trajectories"
   recommendation is withdrawn: it would store the trajectory twice. Index the transcript instead.

**The actual problem is that nothing is indexed.** Three separate full scans:

| Path | Cost |
|---|---|
| `SessionService.SearchTranscripts` | every session × every message in it |
| `memory_search` (both strategies) | `ForEach` over the whole episodic bucket |
| `VectorSearch` | full cosine scan |

### Reference points

nullclaw is far ahead of both here — `src/memory/retrieval/` is a real IR pipeline with a declared
stage order:

```
query_expansion → keyword → vector → merge_rrf → min_relevance
    → temporal_decay → mmr → llm_rerank → limit
```

Worth taking from it:

- **RRF instead of a weighted sum.** The first draft proposed `α·bm25_norm + (1-α)·cosine_norm`.
  That needs both scores normalised onto a comparable scale, and BM25 (unbounded, corpus-dependent)
  against cosine ([0,1]) does not normalise stably. Reciprocal rank fusion sidesteps it entirely by
  merging *ranks*: `Σ 1/(k + rank)`, nullclaw using the canonical `k = 60`. Fewer knobs, no
  calibration, and it degrades gracefully when one source returns nothing. **Use RRF.**
- **Temporal decay as a multiplier**, `score *= exp(-ln2 · age_days / half_life)`, half-life 30d —
  plus nullclaw's `isEvergreen()` exemption, because some memories legitimately must not decay.
  lobslaw's `Retention` field is the natural signal for that.
- **MMR diversity reranking**, so top-K is not three near-duplicates of one memory. Directly relevant
  given Dream clusters near-duplicates but does not always merge them.
- **SimHash + band prefilter for vectors** (see below) — the most transferable idea of the three
  projects.

From hermes, only the negative lesson: it has **no vector store and no embeddings in core at all**,
and its BM25 needed a hand-written C SQLite tokenizer (`native/fts5_cjk/fts5_cjk.c`) plus
watermark-gated triggers to keep an external-content FTS5 index from corrupting. That is a strong
argument *against* SQLite here, not for it.

### Proposal

**1 · Lexical index in bbolt, inside the FSM.** Pure-Go / no-CGO is a recorded decision worth keeping.

```go
BucketTermPostings = "term_postings"  // term -> posting list (doc ID + term freq)
BucketDocStats     = "doc_stats"      // doc ID -> length; plus corpus totals for IDF
```

Indexes **both** episodic records and session messages, under one posting namespace with a doc-kind
prefix, so one query can rank across distilled memory and verbatim transcript.

> **FSM invariant, and the one way to get this wrong:** index updates must happen *inside*
> `FSM.Apply` for the record's own log entry, derived from it — never as a second `raft.Apply`.
> A crash between two applies leaves replicas with different indexes, and a divergent FSM is
> unrecoverable without a snapshot restore.

**2 · Approximate vector search — moved to [R20](#r20--vector-scan-cost)**, because the measured
breakdown says the prefilter has to be designed around the decrypt, not around the arithmetic.

**3 · Fix transcript search.** Five independent fixes, none of which need the index:

| Fix | Detail |
|---|---|
| Tokenise | `SearchTranscripts` matches a single literal lowercase substring, so `"docker compose up"` misses `"docker-compose up"`. `memory_search` already tokenises; make them consistent |
| **Keep the CJK property** | `strings.Index` over lowercased content handles space-free scripts *by construction* — the thing hermes needed a C extension for. Any tokeniser must keep an n-gram path or this regresses. `lingua-go` is already in the tree, so CJK content is expected |
| Use `hit.Matches` | Counted at `session.go:641` and then discarded. Ordering is currently *most-recently-updated session first* with no relevance component at all |
| Best-N snippets | Snippets are the first N in sequence order, so an early throwaway mention beats the substantive later one |
| Source dimension | hermes hit "recall blindness" (#19434) where repetitive cron vocabulary dominated BM25 and starved interactive sessions; it **demotes rather than excludes**. lobslaw will hit this once scheduler-originated turns accumulate and has no source signal to fix it with |

**4 · API.** Implement `SearchRequest.Text` (currently `Unimplemented`), and add
`SearchRequest.Mode = vector | lexical | hybrid`, `hybrid` being the default for the `ContextEngine`
hot path. Keep the existing `strategy` reporting — it is good observability and should name the
fusion, not just the fallback.

**5 · Surfaces**, which also serve R12: `lobslaw history list | show <session> | search <query>`.
The `session_search` / `session_list` / `session_read` builtins already shipped.

### Acceptance

- [ ] An exact-token query ("ERR_2291", a PR number) retrieves the right record where vector search
      does not.
- [ ] No recall path performs a full bucket scan at steady state.
- [ ] A CJK query still matches after tokenisation lands.
- [ ] Transcript hits are ordered by relevance with recency as a decay multiplier, not as the primary key.
- [ ] Top-K contains no two near-duplicate memories (MMR).
- [ ] Scheduler-originated turns are demoted, not excluded, and are still reachable when they are the
      only match.
- [ ] Index rebuild from snapshot produces byte-identical postings on every replica.

---

## R20 — Vector scan cost

Ordered cheapest-first. The first three are hours of work each and need no new concepts; the fourth
is the structural fix. Re-run `search_bench_test.go` after each and record the delta.

### 20a · Store the norm (≈3% of scan time, trivial)

`norm(v.Embedding)` is a property of the stored vector, recomputed on every query for every record.
Add `float32 norm = 11` to `VectorRecord`, populate at write time, fall back to computing it when
absent so old records still work.

### 20b · Decode only what scoring reads (≈20% of scan time, 70 MB/query of allocation)

`proto.Unmarshal` decodes the whole record — `Text`, `metadata`, everything — and scoring reads only
`embedding`, `scope`, `retention`, `owner`, `visibility`. Two options, in preference order:

1. **Split the payload.** Keep the embedding plus filter fields in a narrow `VectorIndexEntry` in its
   own bucket, with the text and metadata in the existing record fetched only for the surviving
   top-K. Best win, and it composes with 20d.
2. Hand-rolled partial decode of just the needed field numbers. Cheaper to build, uglier, and it
   couples to field numbering.

### 20c · Bounded top-K instead of sort-everything (≈1% time, O(N) → O(K) memory)

`vectorSearch` appends every passing record then `sort.Slice`s the lot for a top-3 result. A
`container/heap` of size K is O(N log K) time and O(K) memory. The time win is small; the allocation
win is not, and allocation is what is generating 138 MB of GC pressure per query.

### 20d · Prefilter that never touches the sealed payload (the structural fix)

nullclaw's `SqliteAnnVectorStore` computes a 64-bit SimHash per embedding, splits it into 4× 16-bit
bands, prefilters by band match, then runs exact cosine on candidates, falling back to an exact scan
when candidate recall is insufficient (`ANN_DEFAULT_CANDIDATE_MULTIPLIER = 12`,
`ANN_DEFAULT_MIN_CANDIDATES = 64`).

For lobslaw the band must be reachable **without decrypting anything**, or it saves nothing:

```go
BucketVectorBands = "vector_bands"  // key: "<hmac(band_i)>:<record_id>" -> nil
```

- The band index is a **key-space** structure, so a lookup is a bbolt prefix seek — the
  `ForEachPrefix` pattern already in the tree.
- **Band values are HMAC'd with the cluster MemoryKey.** bbolt *keys* are not sealed, and a raw
  SimHash band is a coarse content fingerprint: someone with the file but not the key could learn
  which records are semantically similar, or test a guessed plaintext against it. HMAC is
  deterministic, so exact-match band lookup still works — banding needs equality, never ordering —
  and the fingerprint stops being plaintext. This costs nothing and closes a leak that the obvious
  implementation would open.
- Built inside `FSM.Apply` for the record's own entry, per the R6 invariant.
- Recall must be **measured against exact scan**, with automatic fallback when it degrades. An ANN
  that silently loses recall is worse than a slow exact scan, because the failure is invisible.

### Also worth fixing here

- **Delete the "fine for personal scale (< ~100k records)" comment** at `search.go:47`. It is off by
  roughly three orders of magnitude in cost terms and it is the reason this was never looked at.
- **Add a minimum score floor.** Nothing anywhere applies one, so a query with no good match still
  injects the top-3 least-bad records into the prompt as *"Relevant context from prior
  conversations"*. Cosine 0.1 noise presented as relevant context is an effectiveness bug, not a
  performance one. `TestCosineAccumulationPrecision` notes the precision caveat that becomes relevant
  once an absolute threshold exists (float32 accumulator, measured rel. error 4.6e-07 at D=4096 —
  fine for a floor, worth knowing about).
- **Surface dimension mismatch.** `vectorSearch` silently skips records whose width differs from the
  query. Changing embedding model therefore makes the entire existing corpus invisible with no error,
  no warning and no metric. Count them and log once per query at WARN.

### Acceptance

- [ ] `BenchmarkVectorSearch/N=10000` improves by at least an order of magnitude.
- [ ] Allocation per query is O(K), not O(N).
- [ ] Band lookup performs no `crypto.Open` on non-candidate records — asserted, not assumed.
- [ ] Band keys are not raw content fingerprints.
- [ ] ANN recall vs exact scan is measured, with automatic fallback on degradation.
- [ ] A dimension-mismatched corpus produces a warning, not silence.
- [ ] A query with no good match returns nothing rather than the least-bad three.

---

## R21 — Embedding outbox

### Problem

`episodic_ingest.go:156`:

```go
if vec, verr := i.embedder.Embed(ctx, embedText); verr == nil {
```

An embedding failure is **silently skipped**. The `EpisodicRecord` is written; its `VectorRecord` is
not. There is no retry, no queue, and **no backfill** — `builtin_memory.go:308` reasons about "once
the backfill runs", but nothing implements one.

So an embedding-provider outage permanently leaves those turns semantically unsearchable. Nothing
reports it, and the only thing masking it is the substring augment path in `memory_search` — which
means that fallback is compensating for a **durability** gap, not the ranking gap it was designed for.

This is the second silent path to unsearchable memory, alongside the dimension-mismatch skip in R20.

### Proposal

nullclaw solves this with `src/memory/vector/outbox.zig` plus `circuit_breaker.zig`. Same shape here,
Raft-resident:

```go
BucketEmbedOutbox = "embed_outbox"  // key: "<ts>:<record_id>" -> pending entry
```

- Ingest writes the outbox entry in the **same Raft entry** as the episodic record, so "record exists
  without embedding" is never an unrecorded state.
- **Dream drains it** — it already runs periodically and is already leader-gated, so no new scheduler
  machinery. Successful embed writes the `VectorRecord` and deletes the outbox entry.
- **Circuit breaker** on the embedding provider: consecutive failures back off rather than retrying
  every cycle. Composes with R8's provider health tracking — same concept, same place to put it.
- Entries carry an attempt count; past a threshold they are marked `failed` and **stay visible**
  rather than being dropped. A queue that silently empties itself reproduces the bug it exists to fix.
- `debug_memory` (or the R12 surface) reports outbox depth and failed count, so "recall is degraded"
  is observable instead of inferred.

**Backfill is the same mechanism.** Records that predate embeddings, or that were skipped during an
outage, are enqueued by a one-shot scan — so 21 delivers the backfill that `builtin_memory.go`
already assumes exists.

### Acceptance

- [ ] An embedding-provider outage during ingest leaves a pending outbox entry, not a silent gap.
- [ ] Dream drains the outbox and the affected turns become semantically searchable with no operator
      action.
- [ ] A persistently failing entry is visible as failed, never silently discarded.
- [ ] A one-shot backfill enqueues every episodic record lacking a vector.
- [ ] Outbox depth is observable.

---

## R19 — Sign and pin the skill handler

🔴 **Exploitable today. Independent of R15 and R18 — do not wait for either.**

### Problem

The skill supply-chain story protects the manifest and not the code:

| Artefact | Signed? | Pinned? |
|---|---|---|
| `manifest.yaml` | ✅ detached ed25519 (`signing.go: readSignature` → `Verify(data, sig)`) | ✅ `Skill.SHA256` — *"hex-encoded manifest-file digest"* (`skill.go:140`) |
| **handler script** | ❌ | ❌ |
| reference files | ❌ | ❌ |

`Manifest.Handler` is a *relative path*. The `SHA256` field that does exist in the manifest
(`skill.go:108`, checked at `:272` as `b.SHA256`) belongs to **binary declarations**, not the handler.

**So: replace the handler script, leave `manifest.yaml` untouched, and both signature verification
and the digest check still pass.** The manifest is protected; the code that executes is not.

`lobslaw-skill-trust` claimed *"a SHA-256 of the manifest+handler tree"* — the shipped code covers
only the manifest, and that decision has been amended to say so.

### Proposal

- Add `handler_sha256` plus per-reference-file digests to the manifest, and make them **required**
  under `SigningPrefer` and `SigningRequire`.
- Extend the signed payload from manifest bytes to a canonical digest root over
  `name + version + manifest digest + handler digest + sorted(file digests) + declared binary digests`
  — the same `approved_root` as `lobslaw-skill-approval-lifecycle`, computed once and used for both
  signature verification and approval pinning.
- Verify at parse **and** immediately before invoke, so a post-load tamper is caught.
- Under `SigningOff`, still compute and record the root so the approval pin is meaningful without
  signatures.

**Migration:** existing unsigned skills gain digests on first parse (trust-on-first-use). Signed
skills need re-signing to gain handler coverage — so `SigningRequire` deployments need a grace mode
that warns on manifest-only signatures before it starts rejecting them.

### Acceptance

- [x] A modified handler fails verification with the manifest untouched.
- [ ] A modified reference file fails verification. *Only the handler is pinned; a skill that reads adjacent data files is still unprotected.*
- [x] Tamper between load and invoke is caught before execution.
- [x] ~~A manifest-only signature warns under a grace flag~~ — no grace flag. A signed manifest that pins no handler is rejected outright. The flag existed to migrate a corpus of already-signed skills; there isn't one.

---

## R7 — Principal identity

Addresses review §3.6.

> **Partially shipped, 2026-08-15.** Ownership and scoping landed ahead of this
> item because a leak forced them (PRs #11, #12); see the aide decisions
> [`user-scoping-ownership-model`](#) and
> [`operator-role-and-cluster-authorization`](#). What shipped is a **subset**
> of the design below, and one part of it sits slightly differently — read the
> gap list before building the rest.
>
> **Done**
>
> - The channel bug named below is fixed. `maybeIngestTurn` took `turn.Channel`
>   from `Claims.Scope`, so every episodic record was tagged `channel:admin`
>   and `ChatID` was never set at all. Both now come from the request, with a
>   regression test.
> - Records carry an `owner` (a principal reference) and a `visibility`, and
>   every read is scoped to an audience whose zero value matches nothing.
> - Cross-channel resolution *works*, via a static `[identity.aliases]` map in
>   `internal/identity`.
>
> **Gaps, in the order they should be closed**
>
> 1. **Identity is config, not state.** `[identity.aliases]` is a flat
>    `channel-id → canonical-id` map read at boot. The design below is right
>    that this belongs in `BucketUserPrefs` as the canonical store with
>    `repeated ChannelIdentity`, plus `BucketPrincipalIndex` for O(1)
>    resolution. Until then, binding a new channel means editing a file and
>    restarting, and there is no `verified_at` / `verified_by` provenance.
> 2. **Resolution happens in the wrong place.** Today `Agent.turnIdentityFor`
>    resolves per turn. The design below resolves at the **gateway edge**, so
>    `Claims.UserID` is already canonical and nothing downstream can forget to
>    resolve. That is the better placement and it should move — the current
>    one works only because every path into the agent happens to go through
>    `runLoop`.
> 3. **No pairing flow.** Nothing binds a new channel address to an existing
>    principal at runtime. The parameters below (8 chars, unambiguous alphabet,
>    crypto/rand, 1h TTL, rate limited, never logged, raft-backed pending set)
>    still stand.
> 4. **No migration from `UserIDScopes`.** Existing entries do not seed
>    identities, so a deployment using them gets no aliasing until an operator
>    writes the map by hand.
> 5. ~~**Records written before ownership are unowned** and therefore readable
>    by everyone.~~ **Closed (PR #18).** The carve-out is gone: an unowned
>    record is now readable by nobody, and episodic ingest refuses a turn with
>    no owner rather than writing one. No `claim` command was built and none is
>    needed — lobslaw has never been deployed, so there is no pre-ownership
>    data anywhere to migrate.
> 6. **Nothing writes `SHARED` at boot.** *Partially closed (PR #20):*
>    `lobslaw memory share` / `unshare` let an operator mark records shared by
>    hand, so the enum is no longer write-only. What is still missing is
>    declarative `[[memory.shared]]` seeding, so a fresh node comes up with
>    operator knowledge already visible to everyone instead of needing a
>    post-boot CLI pass.
> 7. **The cluster gRPC has no principal.** `MemoryService.Search` and an
>    empty-requester `Forget` are unrestricted, spelled out at each site. The
>    design is recorded in `operator-role-and-cluster-authorization`; it is not
>    built.
>
> When items 1–4 land, this note and the config-only alias map should both go —
> two identity models coexisting quietly is exactly what the design below warns
> about.

### Problem

Three unrelated notions of "who":

| Path | Identity |
|---|---|
| Telegram | `UserIDScopes` config: `telegram user_id → scope` |
| REST | JWT claims → `types.Claims{UserID, Scope, Roles}` |
| notify | canonical `user_id` + `BucketUserPrefs` with channel addresses |

Nothing joins them, so cross-channel continuity is impossible by construction: the same human on
Telegram and REST is two unrelated subjects, with two histories and two sets of grants.

Concrete bug in the same area — `internal/compute/agent.go`, in `maybeIngestTurn`:

```go
turn.UserID  = req.Claims.UserID
turn.Channel = req.Claims.Scope   // ← channel populated from scope
```

Every channel-filtered query over episodic records is wrong as a result.

### Proposal

**Extend, don't add.** `BucketUserPrefs` is already the canonical-user store and
`UserPrefsService.FindByChannelAddress(channel, address)` is already the reverse lookup. Promoting
that to *the* identity model beats introducing a parallel `Principal` bucket that would immediately
be a second source of truth.

```proto
// extends existing UserPreferences
message ChannelIdentity {
  string channel    = 1;  // "telegram" | "oidc:https://idp…" | "webhook"
  string address    = 2;  // user_id, JWT sub, …
  google.protobuf.Timestamp verified_at = 3;
  string verified_by = 4;  // "config" | "pairing" | "jwt"
}
// UserPreferences.identities += repeated ChannelIdentity
// UserPreferences.user_id is the canonical principal ID
```

Add `BucketPrincipalIndex` keyed `"<channel>:<address>" → user_id` so resolution is an O(1) `Get`.
`FindByChannelAddress` currently walks `List`, which is fine at one user and not at a hundred.

**Resolution at the gateway edge**, one function for every channel:

```
inbound(channel, address) → PrincipalIndex.Get → user_id
                          → types.Claims{UserID: user_id, Scope, Roles}
```

JWT `sub` resolves through an identity with `channel = "oidc:<issuer>"`, so a token and a Telegram
account can map to one principal. Unresolvable inbound falls back to today's behaviour
(`UnknownUserScope`, or drop).

**Pairing to bind a new channel.** Neither a static `UserIDScopes` map nor JWT-only works for "add
my Signal account". Both references solved this and their parameters are sensible — take hermes's
(8 chars from a 32-char unambiguous alphabet excluding `0/O/1/I`, crypto/rand, 1h TTL, rate limited
per user, lockout after repeated failures, code never logged) over nullclaw's 6 digits. Approval via
`lobslaw pairing approve <channel> <code>`; the pending set lives in Raft so any node can approve.

**Fix the channel bug** in the same commit: thread `req.Channel` into `turn.Channel` and keep
`Claims.Scope` for scope. Add a regression test asserting a channel-filtered episodic query returns
records from that channel only.

**Migration.** At boot, existing `UserIDScopes` entries seed identities with
`verified_by = "config"`. Config stays authoritative for those; no operator action needed.

### Acceptance

- [ ] The same human on Telegram and REST resolves to one principal with one recall view.
- [ ] `verified_by = "config"` identities survive a restart without duplication.
- [ ] Pairing binds a new channel address to an existing principal; codes expire and rate-limit.
- [ ] Channel-filtered episodic queries return only that channel's records.

---

## R8 — Unified provider selection + fallthrough

Addresses review §5. **The framing in the review undersold what's already here** — worth correcting
before proposing anything.

### Problem

lobslaw's `Resolver` is already *more* capable than nullclaw's `model_routes` or hermes's
`hermes model`: chains triggered by complexity score and domain tags, a trust-tier floor
(`MinTrustTier`, `ErrNoProvider`), and multi-step chains with per-step roles and prompt templates
for primary/reviewer handoff. Copying either reference's flat hint→model map would be a downgrade.

The actual problem is that there are **three independent selection paths that share nothing**:

| Path | Used by | Selects on | Failover |
|---|---|---|---|
| `Resolver.Resolve` | main turn | chain triggers, trust floor | none |
| `RoleMap.For` | preflight, reranker, summariser | static label map + hardcoded fallback chain | none |
| `SelectByCapability` | vision, audio, pdf, embeddings builtins | capability + priority | **first match only** |

`SelectByCapability`'s own comment admits the gap: *"the returned slice is kept ordered for the
future fallback-chain layer that will try each in turn"* — and `DEFERRED.md` records the
consequence, that a rate-limited vision provider fails the turn while a configured alternative sits
idle. `DEFERRED.md` deferred it for want of a retry policy. That's the thing to decide.

### Proposal

One selection pipeline; the three existing entry points become thin front-ends over it. This is the
"expanded modality/chained fallthrough" option rather than the hermes/nullclaw hint-map option —
it keeps chains as the source of truth and adds the failover layer they were always missing.

```
Query{ role, capabilities[], min_trust_tier, complexity, domains[], scope }
   │
   ├─ filter   capability ∧ trust floor ∧ scope
   ├─ rank     chain trigger > priority > health > cost class
   ├─ plan     []Attempt — ordered candidates, not one winner
   └─ execute  attempt in order; classify failure; advance or abort
```

**Chains keep their meaning; a chain *step* gains depth.** Today a step resolves to one provider.
It should resolve to an ordered candidate list, so a step can fall through without abandoning the
chain — a reviewer step whose provider is rate-limited retries on the next candidate instead of
killing a multi-step turn.

**Error classification is the policy `DEFERRED.md` was missing.** Guessing isn't needed; the
failure mode tells you what to do:

| Failure | Action | Health effect |
|---|---|---|
| 429 + `Retry-After` | advance to next candidate | demote for `Retry-After` |
| 5xx, connection reset | advance | short cooldown, exponential on repeat |
| 401/403 | advance | **long** cooldown + ERROR log — config problem, not transient |
| 400, context-length, content filter | **abort** — no candidate will do better | none |
| caller's context cancelled/deadline | abort | none — not the provider's fault |

**Health tracking** (`internal/compute/health.go`): per-provider circuit breaker — consecutive
failures, cooldown, half-open probe. Feeds the rank stage, so a flapping provider is skipped rather
than tried-and-failed on every turn. In-process is sufficient (each node's view of provider health
is legitimately its own); no Raft.

**`RoleMap` becomes a query preset.** `RolePreflight` → `Query{capabilities: [chat], prefer: cheap}`,
keeping the existing preflight → main fallback semantics but gaining failover for free.

**Modality builtins pass the full candidate list**, closing the `DEFERRED.md` item.

**Hints as sugar, not as the model.** For config approachability, accept
`hint = "fast" | "balanced" | "deep" | "reasoning" | "vision"` (nullclaw's vocabulary), mapped to
built-in chains that operators can override. Documented explicitly as sugar over chains, so there's
one mental model and not two competing ones.

**Observability.** Every attempt emits an audit entry: provider, latency, outcome, cost. That's what
makes degradation visible — the operational data `DEFERRED.md` said was missing before a retry
policy could be chosen. Also enables the deferred pricing auto-pull, since cost per attempt is now
recorded in one place.

**Sequencing:** land it as a refactor behind the three existing call sites, keeping their signatures,
so the existing resolver/capability/roles tests stay green throughout.

### Acceptance

- [ ] A vision call whose primary provider 429s succeeds on the next configured provider.
- [ ] A 400 / content-length error aborts immediately without burning the candidate list.
- [ ] A provider returning 401 is demoted with a distinct, actionable log line.
- [ ] A chain step falls through without abandoning the chain.
- [ ] Trust-tier floor is honoured at every candidate, not just the first.
- [ ] `hint = "deep"` resolves through a chain an operator can inspect and override.

---

## R9 — Hardline floor + protected paths

Addresses review §4.

### Problem

lobslaw's policy engine is default-deny, which is right — but it is operator-configurable all the
way down, so there is no floor a misconfiguration or a persuasive prompt can't reach. hermes keeps
an unbreakable one: patterns refused even under `--yolo`, `approvals.mode: off`, or explicit
allowlisting, because *"there is no override flag"* for `rm -rf /` and fork bombs.

Nor is there a credential-store denylist independent of sandbox configuration.

### Proposal

**`internal/policy/hardline.go`** — compiled-in, non-configurable, evaluated in `Executor.Invoke`
*before* rules load, and in the shell builtin's argument inspection:

- filesystem wipes (`rm -rf /`, `rm -rf /*`, and `--no-preserve-root`)
- fork bombs
- `mkfs*` / `dd of=/dev/[sh]d*` against mounted block devices
- pipe-from-network-to-interpreter (`curl … | sh`, `wget -O- … | bash`)
- `chmod -R 777 /`, recursive `chown` to root at `/`
- writes to lobslaw's own state: `state.db`, mTLS key material, the credentials bucket path

Returns a distinct error rendered to the model as a tool error, so it can adapt rather than retry
blindly. **Test that no configuration disables it** — that test is the actual feature.

**Protected paths**, enforced in the FS builtins and shell argument inspection, independent of
whatever sandbox policy is in force: `~/.ssh` (except `config`, which routes to confirmation —
hermes's carve-out is correct, there's no key material in it), `~/.aws`, `~/.kube`, `~/.gnupg`,
`/etc/shadow`, `/etc/sudoers`, `.env*`, `.envrc`, and lobslaw's own credential paths.

**State the limitation in the docs, as hermes does:** a shell can still reach these paths by other
means. The denylist reduces accidents and gives the model an unambiguous stop signal; the Landlock +
seccomp sandbox is the real boundary. Claiming otherwise would be worse than not having it.

### Acceptance

- [ ] Every hardline pattern is refused with every policy rule set to allow and confirmations off.
- [ ] A test asserts no config path disables the floor.
- [ ] `~/.ssh/id_*` reads are refused; `~/.ssh/config` prompts.
- [ ] Docs state plainly what the denylist does and does not protect against.

---

## R10 — Channel-agnostic Responder

Addresses review §3.7 — generalising what already works. **Blocks R11.**

### Problem

The interaction quality in `telegram_responsiveness.go` is genuinely better than anything documented
in either reference: typing-indicator refresh inside Telegram's 5s clear window, an interim
"still working" at 30s, a 90s hard cap, and the whole thing gated on the SOUL's `Directness` score
so a terse personality doesn't emit filler.

All of it is Telegram-only. REST gets none of it, and every future channel would reimplement it —
or, more likely, not.

### Proposal

Lift the pattern behind an interface, so responsiveness and confirmation rendering are written once:

```go
// internal/gateway/responder.go
type Responder interface {
    Typing(ctx context.Context) error                     // no-op where unsupported
    Interim(ctx context.Context, text string) error       // "still working" / progress
    Final(ctx context.Context, text string) error
    Prompt(ctx context.Context, p Prompt, scopes []PromptScope) error  // R2's four options
}
```

| Channel | Implementation |
|---|---|
| Telegram | typing action, `editMessageText` for interim, inline keyboard for prompts |
| REST | SSE / chunked streaming when the client accepts it; no-op + poll when it doesn't |
| Webhook | no-op except `Final` |

The responsiveness guards then move up into the shared turn path, taking a `Responder` instead of a
`*TelegramHandler`. Every channel inherits the timers, and `Prompt` is one implementation per
channel instead of one bespoke flow per channel.

### Acceptance

- [ ] A slow REST turn streams interim progress where the client supports it.
- [ ] Responsiveness timers are tested once, against a fake `Responder`.
- [ ] Adding a channel requires no new timer code.

---

## R11 — Channel breadth

Addresses review §5. Depends on R10.

hermes has 6 platforms from one gateway; nullclaw has 19 plus external channel plugins. lobslaw has
three, and `internal/node/wire_gateway.go` is a `switch` over `"telegram" | "webhook" | "rest"`.

- **Out-of-tree channel ABI.** In-tree channels don't scale as a contribution surface. nullclaw's
  external channel plugins are the model. lobslaw already has a plugin lifecycle
  (`internal/plugins`, Claude Code-compatible layout) and MCP client plumbing — a channel plugin is
  the same trust problem as a skill, and the ed25519 signing path already exists.
- **Telegram fidelity.** No `edited_message` handling (an edit is currently just ignored), no forum
  topic / thread support, no reply-to threading. nullclaw binds forum topics to distinct agents at
  runtime via `/bind`, no config edit — a good target.
- **Per-binding agent profiles.** One SOUL per deployment today. nullclaw's `agents.list` +
  `bindings` gives per-agent workspace and memory namespace (`agent:<id>`). `internal/soul/
  fragments.go` suggests the substrate for per-scope variants is partly there already.

Not designed further until R10 lands — the `Responder` boundary determines how much a channel
actually has to implement.

---

## R12 — Memory transparency

Addresses review §3.5.

lobslaw's Dream/REM consolidation, merge adjudication and forget-with-cascade is a stronger
substrate than either reference — with no way for a user to see or steer it. Memory that silently
self-modifies and can't be inspected is a trust problem for a privacy-first product, and it is the
one place where both references are ahead on *product* rather than plumbing:

| | Inspect | Prune | Gate writes |
|---|---|---|---|
| hermes | `/journey`, `session_search` | `journey delete\|edit` | `memory.write_approval` |
| nullclaw | `history list\|show` | — | — |
| lobslaw | — | `Forget` (API only) | — |

Proposed, mostly falling out of R6's surfaces:

- `lobslaw memory ls | show | forget`, with `--quarantined` from R5.
- A consolidation log: what Dream merged, superseded or pruned, and why — the `MergeDecision`
  verdict and reason are already computed and then discarded.
- Optional `memory.write_approval` (hermes's key name) staging agent-initiated writes for approval,
  reusing R2's prompt machinery.

---

## R13 — Progressive skill disclosure

Do this first of the self-learning group. Independent of the rest, pays off at current scale, and
it is the precondition for a skill library that grows on its own.

### Problem

`internal/compute/agent.go` advertises every registered tool on every turn, with the reasoning
recorded in place:

> The keyword-based tailor caused recurring "I don't have that tool" hallucinations whenever a
> category missed; at our current scale (~50 tools, ~5K tokens of definitions) the token cost of
> full advertisement is acceptable. When tool count crosses ~100 we swap to semantic top-K
> retrieval against the existing embedding service.

Semantic top-K reintroduces exactly the failure that killed keyword tailoring — a retrieval miss
still makes a capability invisible, and the model still confabulates about what it has. Ranking
better is not the same as not hiding things.

And once R15 lands, the skill library grows without operator involvement, so "full advertisement is
acceptable" stops being true on a timeline we don't control.

### Proposal

Three-level disclosure, applied to **skills** — not to tools. Tools are the verb set and must stay
fully visible; skills are documents, and a document's *body* is what's expensive.

| Level | Call | Cost |
|---|---|---|
| 0 | Skill index in the system prompt: name + one-line description | bounded, ~60 chars/skill |
| 1 | `skill_view(name)` → the SKILL.md body | on demand |
| 2 | `skill_view(name, path)` → a `references/` file | on demand |

**Nothing becomes invisible.** The index always lists every available skill, so the hallucination
mode can't recur — only bodies are lazy. That is the property semantic top-K cannot offer.

Supporting changes:

- **Cap descriptions at parse, not at render.** hermes truncates to 57 chars when building the index;
  truncating at render means an operator writes a 200-char description and silently loses it.
  Enforce at manifest parse so the error is visible where it can be fixed.
- **Manifest advertises its own reference files** (`references/`, `templates/`, `scripts/`) so
  level 0 can say *what* is available without reading any of it.
- **Conditional activation** so irrelevant skills leave the index entirely. `RequiresBinary` already
  exists; add platform and required-capability gating (hermes's `requires_toolsets` /
  `fallback_for_toolsets` / `platforms`). A skill that needs a vision provider on a text-only
  deployment is noise.

### Acceptance

- [ ] Index cost is O(skills) in *names*, independent of body size.
- [ ] Every installed skill appears at level 0 regardless of the user's message.
- [ ] A skill whose gating fails is absent from the index and cannot be `skill_view`n.
- [ ] Over-long descriptions fail at parse with the offending manifest named.

---

## R14 — Pinned tier-0 memory

### Problem

lobslaw has an archive (vector + episodic, Dream-consolidated) and dynamic per-turn recall. It has
nothing **always-on**. Two consequences:

- Facts that must never be missed are subject to a retrieval hit. A vector miss on "the user prefers
  terse replies" means the turn behaves as if it were never said.
- Dynamic injection is cache-hostile by construction: the prompt changes every turn.

hermes's answer is two small capped files (`MEMORY.md`, `USER.md` — 2,200 and 1,375 chars) injected
as a **frozen snapshot at session start**, with the reason stated in `tools/memory_tool.py`:

> Mid-session writes update files on disk immediately (durable) but do NOT change the system prompt
> — this preserves the prefix cache for the entire session.

The small size is not modesty. It is a fixed tax on every request, so it must be small; and the cap
is what forces curation, since *"memory does not auto-compact"* over there.

### Proposal

Two capped, pinned blocks: **user profile** (who the user is) and **agent notes** (environment
facts, conventions, learned quirks). Rendered by new `promptgen.BuildUserProfile` /
`BuildAgentNotes` sections, positioned in the stable prefix.

- **Frozen per session.** Writes are durable immediately; the rendered snapshot refreshes at the
  next session boundary. Prefix stability is the point.
- **Character caps, not token caps** — char counts are model-independent. hermes's 2,200 / 1,375 are
  a reasonable starting reference; make both configurable under `[memory.pinned]`.
- **Storage.** User half extends `BucketUserPrefs`, which already exists and is already canonical
  for per-user state. Agent notes get a sibling record.
- **Entries are delimiter-separated and multiline**, edited by short unique substring match rather
  than IDs — what makes the tool usable without maintaining an index the model has to read first.

**Where we improve on hermes.** Their cap forces the model to consolidate *in the same turn*,
because they have no background consolidator. lobslaw has Dream. So:

- Overflow still **errors** rather than truncating — the pressure is the feature.
- At ~80% capacity, Dream proposes a consolidation asynchronously, off the hot path. The cap creates
  the pressure; Dream does the work.

**Copy their scar regardless.** `tools/memory_tool.py` carries
`_MAX_CONSOLIDATION_FAILURES_PER_TURN = 3`, added after issue #42405:

> so a fragile replace/add can't loop the turn to budget exhaustion and **suppress the user's reply**

A hard cap plus "consolidate now" is a livelock waiting to happen. After N failures, return a
terminal result that tells the model to stop and answer the user. A memory side effect must never
cost the user their reply.

**R5 interaction.** These blocks are agent-written and land in system position, so they must be
`promptguard`-scanned on write even though the store is trusted. Provenance of the *store* is not
provenance of the *content*.

### Acceptance

- [ ] Prompt prefix is byte-identical across turns within a session (cache-hit assertion).
- [ ] A write past the cap errors with current usage and leaves the store unchanged.
- [ ] Dream proposes a consolidation at the configured threshold without blocking a turn.
- [ ] N consecutive consolidation failures yield a terminal result; the user still gets a reply.

---

## R15 — Self-taught store

**Foundation for R16 and R17.** Nothing self-authored ships until this exists.

### Intent

**Provenance by location, not by tag.**

hermes tracks write origin with a `ContextVar` (`tools/skill_provenance.py`) because every skill
lives in one directory, so origin has to be carried out of band. The curator then has to *remember*
its invariant — *"Only touches agent-created skills"* — as a rule it applies, and the review fork
has to be told in its prompt which skills are off-limits.

A separate store makes all of that structural instead of remembered:

| Property | With a separate store |
|---|---|
| Provenance | If it's in the store, the agent wrote it. No marker to forget, forge, or lose |
| Disable | A capability boundary, not a branch: store unwired ⇒ no write path exists |
| Blast radius | "Forget everything you taught yourself" is one operation |
| Audit | "Show me everything it decided on its own" is one namespace scan |
| R17's invariant | Free — the curator's domain *is* the store |

### Boundary rule — what goes in

The store holds **behaviour-changing artefacts the agent authored**: skills, procedures, and
proposed pinned-block entries. Observations continue to flow into the episodic and vector buckets
exactly as they do now.

The line is: *does this change behaviour without retrieval?* An episodic record is data the model
*may* retrieve and weigh. A skill or a pinned entry is an instruction it *follows*. Those deserve
different custody, and the risk profiles are not comparable.

### Shape

```go
// internal/memory/buckets.go — new
BucketSelfTaught        = "self_taught"          // artefact records
BucketSelfTaughtUsage   = "self_taught_usage"    // telemetry (see below)
BucketSelfTaughtArchive = "self_taught_archive"  // archived, never deleted
```

```proto
message SelfTaughtRecord {
  string id   = 1;
  SelfTaughtKind kind = 2;   // SKILL | PROCEDURE | PINNED_PROPOSAL
  string name = 3;
  string body = 4;
  map<string, string> files = 5;   // references/… -> content
  SelfTaughtOrigin origin   = 6;   // REVIEW_FORK | USER_DIRECTED
  string turn_id    = 7;           // what taught it — audit correlation
  string session_id = 8;
  SelfTaughtState state = 9;       // PROPOSED | ACTIVE | STALE | ARCHIVED
  bool   pinned  = 10;             // orthogonal: opts out of all auto-transitions
  uint32 version = 11;
  google.protobuf.Timestamp created_at = 12;
  google.protobuf.Timestamp updated_at = 13;
}
```

**Materialisation and precedence are owned by [R18](#r18--skills-in-the-cluster-store)**, which
generalises them to every skill rather than only self-taught ones. R15 contributes the
`BucketSelfTaught` source and the `agent` tier; R18 owns the store-to-cache contract, the version
history, and the tier-first winner rule that makes "never shadows a signed or operator skill"
actually true.

The earlier draft of this section proposed materialising self-taught skills onto their own storage
mount while signed and operator skills stayed filesystem-authoritative. That created two sources of
truth and a reconciliation rule between them. R18 removes the split instead of arbitrating it.

### Provenance tiers

| Tier | Source | Policy default |
|---|---|---|
| `signed` | clawhub bundle, ed25519 verified against a trusted publisher | full manifest capabilities |
| `operator` | on-disk, operator-authored | full |
| `agent` | written by the review fork | no new binaries, MCP servers, credentials, or network declarations; sandbox floor; confirmation on first invoke; never shadows the tiers above |

### Archive, never delete

Deletion is not a lifecycle transition. Archived artefacts move to `BucketSelfTaughtArchive` and
stay recoverable. For a product whose pitch is trust, an agent that can silently erase evidence of
what it taught itself is the wrong default — and hermes reached the same conclusion
(*"Never auto-deletes — only archives. Archive is recoverable."*).

### Telemetry: a bucket, not a sidecar

hermes keeps usage counters in a sidecar `~/.hermes/skills/.usage.json` to avoid conflict pressure
on user-authored SKILL.md files. lobslaw wants a bucket instead, for three reasons that are stronger
than theirs:

1. **Signature validity.** Counters inside a manifest would invalidate its digest. For hermes this
   is tidiness; here it breaks verification.
2. **Cluster aggregation.** A per-node JSON file is invisible to peers, so every node but one would
   compute staleness from partial data — and R17's transitions depend on it. hermes never faces this.
3. **Atomicity.** The FSM provides it. hermes needs `fcntl`/`msvcrt` locking plus a
   "broken sidecar never breaks the tool call" degradation path.

> **Do not `raft.Apply` per `skill_view`.** Counter bumps are high-frequency and low-value; paying
> consensus for each one is the obvious way to make this worse than the sidecar it replaced. Batch
> in-process and flush on turn end or every N seconds. Losing a handful of counts to a crash is
> acceptable — hermes already treats these as best-effort — losing the write path to contention is
> not.

### The self-learning switch

Three states, because "on/off" is the wrong shape for a security-first product:

```toml
[self_learning]
mode = "propose"   # "off" | "propose" | "auto"
```

| Mode | Behaviour |
|---|---|
| `off` | Store not wired. No fork spawns, no artefact loads, no write path exists. **Verifiable by absence**, not by a branch someone has to have remembered to check |
| `propose` | Artefacts land `PROPOSED` and do not load until approved — via R2's prompt machinery or `lobslaw learned review`. hermes's `write_approval` generalised, and the right default here |
| `auto` | Artefacts become `ACTIVE` immediately (hermes's default) |

`off` must be enforced at wiring — `wireSelfTaught` returns nil and every dependent is absent. A
feature flag that merely guards call sites is a different, weaker claim than "the capability is not
present", and the second is what an operator disabling self-learning is asking for.

### Acceptance

- [ ] With `mode = "off"`, no self-taught bucket is opened and no self-taught artefact can load.
      Asserted by wiring, not by mocking a flag.
- [ ] A self-taught skill never shadows a signed or operator skill of the same name.
- [ ] An `agent`-tier skill declaring a new binary or MCP server fails to load, with the reason.
- [ ] `propose` mode artefacts are inert until approved.
- [ ] One command lists, and one command discards, everything self-taught.
- [ ] Usage counters survive a leader change and aggregate across nodes.

---

## R16 — Post-turn review fork

Depends on R15 — the fork's only write target is the self-taught store.

### Proposal

Fork after the reply is delivered, replay the conversation, ask whether anything should be learned.
`maybeIngestTurn` is already the hook point and already establishes the pattern (fire behind the
reply, bounded background context, failures log and are swallowed).

**Asymmetric triggers**, which is the best single idea in hermes's design:

| Axis | Interval | Unit | Rationale |
|---|---|---|---|
| Memory | 10 | conversation **turns** | Measures "have we learned who the user is" |
| Skills | 10 | tool **iterations in one turn** | Measures "was there enough work to be worth encoding" |

A forty-tool-call debugging session triggers a skill review immediately; forty turns of chat don't.
(hermes: `agent_init.py:1744` / `:1860`, checked in `turn_context.py:698` and
`turn_finalizer.py:734`.)

**Recursion guard.** The fork zeroes its own intervals, or the first review spawns the second.

**Suppressed for scheduler-originated turns.** No human in the loop means nothing to learn about the
user, and hermes measures the fork at *~30K tokens per event* — too much to spend on a cron tick.

**Cost-aware replay.** The instinct to route background work to a cheap model is wrong here, and
hermes documents why: the fork on the **main** model replays the full conversation against a warm
prefix cache, so it is mostly cheap cache reads. A *different* model can't reuse that cache, so it
is cold regardless — and replaying the full transcript would cold-write all of it. Therefore:

> Same model → full replay. Different model → compact digest. That's the whole policy.

Add `RoleReview` to `RoleMap` so the routing is explicit, and derive replay mode from whether the
resolved provider matches the turn's main provider.

**Authority is a policy scope, not a runtime denylist.** hermes whitelists the fork's tools and
denies the rest at runtime. lobslaw should express it as a scope in the existing default-deny engine:
write on the self-taught namespace, nothing else. The tool whitelist and the write domain then become
one statement, evaluated by the same engine and recorded in the same hash-chained audit log as every
other decision.

### Prompt design

Adopt from hermes's `_COMBINED_REVIEW_PROMPT`:

- **The preference order.** Patch a currently-loaded skill → patch an existing umbrella → add a
  `references/` support file → only then create a new class-level skill.
- **The anti-sprawl naming rule.** No skill named after a PR number, error string, codename, or
  `fix-X`/`debug-Y` session artifact. Class-level names only.
- **The axis split**, which is the conceptual payload:
  > Memory says *who the user is and what the current state is*; skills say *how to do this class of
  > task for this user*.
  >
  > Frustration and style corrections are first-class **skill** signals, not just memory signals.

  Recording "the user hates verbose replies" changes nothing next session. Encoding it in the
  procedure that governs the task does. lobslaw has a third axis the references lack — SOUL is *how
  to be* — so the split is three-way here: memory = state, skills = procedure, soul = disposition.

**Do not adopt** hermes's action bias — *"a pass that does nothing is a missed learning opportunity,
not a neutral outcome"*, evaluated every ~10 tool iterations. That pressure is precisely what forced
them to build a curator, usage telemetry, a stale/archive lifecycle and protected-skill rules. Bias
conservative by default: lobslaw has Dream to catch what a quiet pass missed, and `propose` mode
means a marginal artefact costs the user an approval prompt.

### Acceptance

- [ ] Review fires after the reply, never before it.
- [ ] Skill review triggers on a tool-heavy single turn; memory review on turn count.
- [ ] A fork cannot spawn a fork.
- [ ] Scheduler-originated turns spawn no review.
- [ ] Routing the review to a non-main provider switches replay to a digest.
- [ ] The fork cannot write outside the self-taught namespace — enforced by policy, shown in audit.

---

## R17 — Self-taught lifecycle (curator)

Depends on R15 (the store and its state field) and R16 (something to curate). Not specifiable
before those land, which is why it sits last.

### Proposal

Lifecycle over self-taught artefacts, driven by the usage telemetry in `BucketSelfTaughtUsage`:

| State | Transition |
|---|---|
| `active` | default |
| `stale` | unused beyond `stale_after_days` (default 30) |
| `archived` | unused beyond `archive_after_days` (default 90); moved to the archive bucket |
| `pinned` | orthogonal boolean — opts out of every automatic transition |

- **Deterministic pruning always on; LLM consolidation opt-in.** hermes ships the umbrella-building
  fork off by default and keeps the inactivity prune running. Same split: state transitions are
  cheap and predictable, merging artefacts is a judgement call that should be a choice.
- **Never delete.** Archive only, per R15.
- **No inactivity trigger needed.** hermes built one because it has no scheduler. lobslaw has a
  leader-gated scheduler and Dream already runs periodically — and Dream already scores, clusters,
  adjudicates merges and prunes for memory. Extending that machinery to artefacts is a smaller step
  here than it was there.
- **Usage must be cluster-aggregated**, which is why R15 chose a bucket over a sidecar: a curator
  computing staleness from one node's view would archive skills another node uses daily.

### Acceptance

- [ ] A skill unused past the threshold transitions to stale, then archives, and stays recoverable.
- [ ] A pinned artefact never transitions.
- [ ] Nothing outside the self-taught store is ever touched — asserted, not assumed.
- [ ] Staleness is computed from cluster-wide usage.

---

## R18 — Skills in the cluster store

Precedes R15. Changes where skills live, not what they are.

Recorded as aide decision **`lobslaw-skill-storage-model`** (2026-08-15), which supersedes the
"skills live in storage as directories" clause of `lobslaw-skills` and `lobslaw-persistence-model`.
The three [open questions](#open-questions) below are carried in that decision and are unresolved.

### The confusion to remove

Files currently play three unrelated roles, all pointed at the same directory:

| Role | Today | Proposed |
|---|---|---|
| **Authority** — what the cluster believes is installed | filesystem, under a storage mount | **Raft / bbolt** |
| **Interchange** — install, share, back up, restore | clawhub tarball, or a directory | import / export, both directions |
| **Execution substrate** — what the interpreter and Landlock see | the same directory as authority | a derived per-node cache, disposable |

Separating *authority* from *execution substrate* is the entire change. Today they are the same
directory, which is why a skill exists only where a mount happens to be materialised.

### Why this is the right call

- **One source of truth.** The alternative — signed and operator skills on disk, agent-authored
  skills in the store — needs a reconciliation rule between two authorities. That complexity is
  self-inflicted.
- **Versioning, rollback, backup and restore fall out of the store**, rather than each needing its
  own filesystem convention. `soul_history_rollback` is the precedent already in the tree.
- **It closes a `DEFERRED.md` item.** "Cross-cluster storage tunneling" records that a compute-only
  node cannot reach mounts materialised on a peer. Skills-in-Raft removes skills from that problem
  entirely: every node has every skill because every node has the log.
- **Provenance-by-location survives.** Separate buckets per source, so the importer writes one and
  the review fork writes the other, and `self_learning.mode = "off"` is still verifiable by absence.
  What unifies is the *runtime*, not the storage.

### Shape

```go
// internal/memory/buckets.go — new
BucketSkills        = "skills"          // imported: signed + operator tiers
BucketSelfTaught    = "self_taught"     // R15: agent-authored
BucketSkillVersions = "skill_versions"  // "<name>:<version>" -> record (history)
BucketSkillBlobs    = "skill_blobs"     // digest -> bytes (small text payloads only)
```

```proto
message SkillRecord {
  string name    = 1;
  string version = 2;
  SkillTier tier = 3;          // SIGNED | OPERATOR | AGENT

  // Manifest bytes VERBATIM, plus its detached signature. Not a
  // parsed struct — see the trap below.
  bytes  manifest_yaml = 4;
  bytes  manifest_sig  = 5;

  map<string, string> files = 6;  // relative path -> blob digest
  SkillSource source = 7;         // clawhub URL / import path / fork turn_id
  string imported_by = 8;         // principal
  google.protobuf.Timestamp imported_at = 9;
  bool active = 10;               // one active version per (name, tier)
}
```

> **Store the manifest bytes verbatim.** The signature is a detached ed25519 signature over the
> exact manifest file (`signing.go: readSignature` reads `manifest.yaml.sig`; `Verify(data, sig)`).
> Parsing into a proto and re-serialising on export changes the bytes and breaks verification
> permanently — the skill becomes unverifiable, and `SigningRequired` deployments would reject a
> skill that was genuinely signed. Round-tripping the bytes untouched is what keeps
> `SigningRequired` working unchanged after the move.

**Blob split, with a threshold.** Every Raft apply replicates to every node and lives in snapshots
thereafter, so the store is the wrong place for multi-megabyte payloads. Manifests, handlers and text
references (kilobytes) go in. Sidecar binaries do not — they stay in storage, content-addressed, with
the digest on the record, which is how `internal/binaries` already resolves them after a clawhub
install. Oversized payloads **fail at import with the offending path named**, rather than being
silently split or silently accepted.

### Store-to-cache contract

Materialisation is not a compromise — execution requires a filesystem. `invoker.buildPolicy` grants
Landlock `Read`+`Exec` on `skill.ManifestDir` so the interpreter can load and run the handler. The
store cannot be `exec`'d.

- Cache lives per node at `<state>/skills-cache/<name>/<version>/`.
- Written **only** by the materialiser. Never edited in place.
- Rebuilt on boot, on record change, on digest mismatch, or on `lobslaw skills rematerialise`.
- **Disposable.** `rm -rf` the cache and restart is a complete recovery. That property is the test of
  whether the store is really the authority.

The implementation insight that keeps this small: **`Registry.Scan` / `Registry.Watch` do not change.**
They point at the cache directory instead of a storage mount. This is a change of *source*, not a
change of registry, invoker, sandbox or policy derivation.

### Precedence must become tier-first

`candidateBeats` currently orders by version, then `preferSigned` only as a tie-break at equal
version, then manifest dir. So **an unsigned v2 beats a signed v1 today.** That is defensible while
nothing but an operator can write a skill; it becomes a privilege-escalation path the moment an agent
can author one — bump the version, win the name.

New order: **tier → version → …**, with `signed > operator > agent`. This is a change to existing
behaviour, so it lands with an explicit test and a note in the commit message rather than quietly.

### Versioning, backup, restore

- Every import or write appends to `BucketSkillVersions`; `active` is a separate pointer.
- `lobslaw skills history <name>` and `lobslaw skills rollback <name> <version>` — deliberately the
  same shape as `soul_history_rollback`, so it is a pattern already known in this codebase.
- **Backup** = a Raft snapshot (already exists), or `lobslaw skills export --all` writing a directory
  tree.
- **Restore** = import that tree, or restore the snapshot.
- Round-trip fidelity is testable and worth pinning: `import → export → diff` must be empty.

### Migration

1. Importer + exporter + materialiser, with the registry still reading its current mount. No
   behaviour change yet.
2. `lobslaw skills import` the existing mount contents; verify the round-trip diff is empty.
3. Point the registry at the cache. The mount becomes an import source.
4. clawhub install becomes download → verify digest + signature → **import**, instead of
   extract-to-mount. Binaries continue through `internal/binaries` unchanged.

**Keep a dev source.** Losing edit-a-file-and-it-reloads would make skill authoring an
edit → export → import loop, which is bad enough that people will work around it. A configured dev
directory keeps today's `Scan`/`Watch` behaviour, always wins locally, is never written to the store
and never replicated, and is logged loudly at boot so it can't be a surprise in production.

**R0 becomes a hard dependency** — `skills install` is a Raft write, so a non-leader gateway or CLI
node needs forwarding.

### Open questions

Not decided here; they need a call before implementation:

1. **Blob threshold**, and whether reference files above it are permitted at all or must move to
   storage.
2. **Is a live-watched operator directory legal in production**, or is dev-mode strictly
   development-only?
3. **Version history is unbounded**, and it inflates every Raft snapshot forever. Needs a
   `keep_versions = N` policy plus GC, or "versioned skills" quietly becomes a store-growth problem.

### Acceptance

- [ ] A node with no storage mount and no skills directory serves the full skill library from the log.
- [ ] Deleting the cache and restarting restores every skill byte-identically.
- [ ] `import → export → diff` is empty for a signed skill, and the export still verifies.
- [ ] `SigningRequired` behaviour is unchanged after the move.
- [ ] A self-taught skill cannot win a name against a signed or operator skill at any version.
- [ ] `skills rollback` restores a prior version across the cluster.
- [ ] An oversized payload fails at import, naming the path.

---

## Cross-cutting notes

**Proto changes.** R1, R2, R6, R7, R15 and R18 all add messages or fields. Batch them into one proto change
per item rather than one omnibus change, and keep every change additive — a rolling upgrade must
have old and new nodes reading the same log.

**FSM determinism.** R6's index is the risky one. Any state derived from a log entry must be built
inside `FSM.Apply` for that entry. A second `raft.Apply` to maintain derived state can diverge
replicas on a crash between the two, and a divergent FSM needs a snapshot restore to fix.

**Documentation.** Per `lobslaw-documentation-diagrams`, each item ships its sequence diagram with
the code. R1 and R2 change flows already drawn in `ARCHITECTURE.md` and `GATEWAY.md`; those diagrams
are updated in the same commit, not after. R4 additionally deletes a TODO from a diagram that
currently documents the bug.

**Test posture.** The existing suite pins several behaviours these items change —
`TestPromptRegistryConcurrentResolveOnlyOneWinner` (R2), the telegram history tests (R1), the
resolver/capability/roles tests (R8). In each case the test's *assertion* should survive with the
implementation swapped underneath it. Where it can't, that's a behaviour change worth calling out in
the commit message rather than quietly rewriting the test.
