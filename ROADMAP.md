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
| **R6** | [Hybrid recall](#r6--hybrid-recall) | 🟠 P1 | L | — |
| **R7** | [Principal identity](#r7--principal-identity) | 🟠 P1 | M | — |
| **R8** | [Unified provider selection + fallthrough](#r8--unified-provider-selection--fallthrough) | 🟠 P1 | M | — |
| **R9** | [Hardline floor + protected paths](#r9--hardline-floor--protected-paths) | 🟠 P1 | S | — |
| **R10** | [Channel-agnostic Responder](#r10--channel-agnostic-responder) | 🟡 P2 | M | R11 |
| **R11** | [Channel breadth](#r11--channel-breadth) | 🟡 P2 | L | — |
| **R12** | [Memory transparency](#r12--memory-transparency) | 🟡 P2 | M | — |

R4 and R5 are P0 on security grounds and are independent of everything else — land them first if
R0/R1 slip. R4 is a handful of lines.

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

- [ ] A three-node cluster where the gateway node is a follower persists sessions, prompts and
      leases with no operator action.
- [ ] Killing the leader mid-turn surfaces a retryable error, not a lost message.
- [ ] `ErrNoLeader` and `ErrNotLeader` are separately testable and separately logged.

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

## R6 — Hybrid recall

Addresses review §3.4.

### Problem

`docs/dev/MEMORY.md`: *"No text-based search. `SearchRequest.Text` returns `Unimplemented`."*
Recall is pure cosine over a full scan, top-3 per turn, with no lexical path and no recency
weighting.

Embeddings are weakest at exactly what users ask: *"what was that error code"*, *"the PR number you
mentioned"*, *"what did we call that function"*. Both references have a lexical path — hermes uses
SQLite FTS5 behind a `session_search` tool; nullclaw's markdown profile does *"hybrid retrieval with
temporal decay (half-life 30 days)"*.

Separately: `EpisodicTurn` stores only `(user message, assistant reply)`. Tool trajectories,
decisions and outcomes — the part worth recalling — are discarded.

### Proposal

**Lexical index in bbolt, inside the FSM.** Not SQLite: the pure-Go / no-CGO constraint is a
recorded decision and worth keeping. A new bucket:

```go
BucketTermPostings = "term_postings"  // term -> posting list (record IDs + term freq)
BucketDocStats     = "doc_stats"      // record ID -> length; plus corpus totals for BM25 idf
```

> **FSM invariant, and the one way to get this wrong:** index updates must happen *inside*
> `FSM.Apply` for the record's own log entry, derived from it — never as a second `raft.Apply`.
> A crash between two applies would leave replicas with different indexes, and a divergent FSM is
> unrecoverable without a snapshot restore. Same rule the existing derived state follows.

BM25 scored at query time from postings + doc stats.

**Fusion.** Normalise both scores to [0,1], then:

```
score = (α · bm25_norm + (1-α) · cosine_norm) · decay(age) · importance_boost
decay(age) = exp(-ln2 · age / half_life)      // half_life default 30d, per nullclaw
```

`α` default 0.4 — vector-leaning, since semantic recall is the existing strength and lexical is
there to catch what it misses. Configurable under `[memory.recall]`.

**API.** Implement `SearchRequest.Text` as lexical-only (it currently returns `Unimplemented`, so
this is additive), and add `SearchRequest.Mode = vector | lexical | hybrid` with `hybrid` the default
for the `ContextEngine` hot path.

**Surfaces**, which also serve R12:

- `session_search` builtin — the agent can answer "did we discuss X last week?" itself.
- `lobslaw history list | show <session> | search <query>` — nullclaw's `history list`/`show <id>`
  shape, which is the right CLI vocabulary.

**Richer episodes.** Extend `EpisodicTurn` with a tool trajectory: tool name, argument **digest**
(never raw args — they carry paths and secrets), outcome, duration. Plus explicit decision capture
when the turn produced one. This is what makes "why did we do it that way" answerable.

### Acceptance

- [ ] An exact-token query ("ERR_2291", a PR number) retrieves the right record where vector search
      does not.
- [ ] Recall favours a recent record over an older near-identical one.
- [ ] Index rebuild from snapshot produces byte-identical postings on every replica.
- [ ] `history search` finds a discussion from a session outside the active window.

---

## R7 — Principal identity

Addresses review §3.6.

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

## Cross-cutting notes

**Proto changes.** R1, R2, R6 and R7 all add messages or fields. Batch them into one proto change
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
