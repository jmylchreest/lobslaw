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

Status is the tree as of 2026-08-15 (see [Status drift](#status-drift) for detail).
✅ done · 🟨 partial · ⬜ not started.

| # | Item | Status | Priority | Effort | Blocks |
|---|---|---|---|---|---|
| **R0** | [Leader-forwarding write path](#r0--leader-forwarding-write-path) | ✅ | 🔴 P0 | S | R1, R2, R3 |
| **R1** | [Session layer](#r1--session-layer) | ✅ | 🔴 P0 | L | R2, R3, R4 |
| **R2** | [Durable, cluster-wide confirmations](#r2--durable-cluster-wide-confirmations) | ✅ | 🔴 P0 | M | — |
| **R3** | [Turn serialisation + inbound queue](#r3--turn-serialisation--inbound-queue) | 🟨 | 🔴 P0 | M | — |
| **R4** | [Policy engine fails closed](#r4--policy-engine-fails-closed) | ✅ | 🔴 P0 | XS | — |
| **R5** | [One trust contract + ingest scanning](#r5--one-trust-contract--ingest-scanning) | ✅ | 🔴 P0 | M | — |
| **R6** | [Retrieval mechanics](#r6--retrieval-mechanics) — *[Retrieval](#retrieval--r6-r20-r21)* | 🟨 | 🟠 P1 | L | — |
| **R20** | [Vector scan cost](#r20--vector-scan-cost) — *[Retrieval](#retrieval--r6-r20-r21)* | 🟨 | 🟠 P1 | M | — |
| **R21** | [Embedding outbox](#r21--embedding-outbox) — *[Retrieval](#retrieval--r6-r20-r21)* | ⬜ | 🟠 P1 | S | — |
| **R7** | [Principal identity](#r7--principal-identity) | 🟨 | 🟠 P1 | M | — |
| **R8** | [Unified provider selection + fallthrough](#r8--unified-provider-selection--fallthrough) | ✅ | 🟠 P1 | M | — |
| **R9** | [Hardline floor + protected paths](#r9--hardline-floor--protected-paths) | ✅ | 🟠 P1 | S | — |
| **R10** | [Channel-agnostic Responder](#r10--channel-agnostic-responder) | ✅ | 🟡 P2 | M | R11 |
| **R11** | [Channel breadth](#r11--channel-breadth) | ⬜ | 🟡 P2 | L | — |
| **R12** | [Memory transparency](#r12--memory-transparency) | ✅ | 🟡 P2 | M | — |
| **R13** | [Progressive skill disclosure](#r13--progressive-skill-disclosure) | ✅ | 🟠 P1 | M | R15, R16 |
| **R14** | [Pinned tier-0 memory](#r14--pinned-tier-0-memory) | ✅ | 🟠 P1 | S | — |
| **R15** | [Self-taught store](#r15--self-taught-store) | ✅ | 🟠 P1 | M | R16, R17 |
| **R16** | [Post-turn review fork](#r16--post-turn-review-fork) | ✅ | 🟡 P2 | M | R17 |
| **R17** | [Self-taught lifecycle (curator)](#r17--self-taught-lifecycle-curator) | ✅ | 🟡 P2 | M | — |
| **R18** | [Skills in the cluster store](#r18--skills-in-the-cluster-store) | ✅ | 🟠 P1 | L | R15, R17 |
| **R19** | [Sign and pin the skill handler](#r19--sign-and-pin-the-skill-handler) | ✅ | 🔴 P0 | S | — |
| **R22** | [Provider / modality layer](#r22--provider--modality-layer) — *[Providers](/dev/PROVIDERS)* | ✅ | 🟠 P1 | L | R23, R24 |
| **R23** | [External drivers as skills](#r23--external-drivers-as-skills) — *[Providers](/dev/PROVIDERS)* | ⬜ | 🟡 P2 | M | — |
| **R24** | [Turn trace export](#r24--turn-trace-export) — *[Trace](/dev/TRACE)* | ✅ | 🟠 P1 | M | — |
| **R27** | [The config sweep](#r27--the-config-sweep-done-2026-08-17) | ✅ | 🟠 P1 | M | — |
| **R25** | [Retire the node functions that select nothing](#r25--retire-the-node-functions-that-do-not-select-anything) | ⬜ | 🟡 P2 | S | — |
| **R26** | [A second vendor per generation modality](#r26--a-second-vendor-per-generation-modality) | ⬜ | 🟠 P1 | M | R22 |
| **R28** | [The operator's laptop](#r28--the-operators-laptop) | ✅ | 🟠 P1 | L | — |
| **R29** | [Enrolling an operator without moving a private key](#r29--enrolling-an-operator-without-moving-a-private-key) | ✅ | 🟠 P1 | M | R28 |
| **R30** | [Cross-network node enrolment](#r30--cross-network-node-enrolment) | ⬜ | 🔵 P2 | M | R29 |

### Bookkeeping (reviewed 2026-08-17)

The status column and the acceptance boxes had drifted apart, in both directions. Corrected:

- **R8 → ✅.** All eight boxes are checked; chains route and their steps run.
- **R18 → 🟨, from ✅.** Marked complete when its CLI landed, but `skills rollback` is on the
  acceptance list and does not exist. Two boxes were ticked against tests that already cover them;
  three describe behaviour that is probably present and has never been verified.
- **R22 → 🟨, from ⬜.** Three generation modalities shipped while this said "not started". The
  driver consolidation genuinely has not.
- **R25 and R26** had sections but no row in the table above, so they were invisible to anyone
  reading the index.

**R1 and R5 are now reconciled** (2026-08-17). Both were marked ✅ with *no* boxes ticked, which
read as criteria written and never verified rather than work left undone. Ticking them from memory
would have been inventing evidence — and checking them found a real bug: R5 claimed a record
containing `</untrusted>` could not escape its block, and it could. Everything else was true, and
three boxes across the two were true in a way nothing tested.

The lesson is worth keeping: a ✅ with unticked boxes is not bookkeeping debt, it is an untested
claim.

**Remaining P0: R2 (durable confirmations), R5 (trust contract + ingest scanning), and the
persisted pending queue in R3.** R2 is now cheap — #29 landed per-record revisions plus
`LogEntry.expected_revision`, which is the CAS primitive its atomic resolve needs.

R13–R17 are the self-learning group, derived from reading hermes-agent's implementation
(`agent/background_review.py`, `agent/curator.py`, `tools/memory_tool.py`,
`tools/skill_provenance.py`, `tools/skill_usage.py`). R13 lands first — it is independent of the
rest and pays off at current scale, and it is what makes a growing skill library affordable.
R15 is the foundation for R16/R17: nothing self-taught is shippable until the store exists.

**R18 changes where skills live at all, and precedes R15.** Sequence for this group:
R13 → R18 → R14 → R15 → R16 → R17. R13 and R14 are independent of R18 and can land in any order
alongside it.

### Status drift

Written before Phase-12-era work landed, and re-verified against the tree on 2026-08-15 after
PRs #21–#30. The table below is what the code actually does; the sections further down are the
proposals, some of which have since been superseded by what shipped. Where the two disagree, the
table wins — and the section says so at its head.

| # | State |
|---|---|
| R1 | **Largely landed** — `BucketSessions` + `BucketSessionMessages`, `internal/memory/session.go`, `internal/gateway/conversation.go`, `compute.ContextBudget`, rolling summary, `session_search`/`session_list`/`session_read`. Note the shipped design keeps messages in their own bucket rather than inline on the session record as proposed below |
| R4 | **Done.** `Engine.Evaluate` returns `EffectDeny`/"no rule matched (default-deny)" at the bottom, denies nil claims, and a rule whose conditions cannot be evaluated is skipped only when it would have allowed — deny and require_confirmation apply anyway |
| R19 | **Done for the handler.** `signing.go` + `ParseWithPolicy` enforce detached ed25519 signatures over `manifest.yaml`; the manifest pins `handler_sha256`, which is what makes the signature cover executable content, and the invoker re-hashes before exec. Signed manifests that pin nothing are rejected. Still open: only the handler is pinned, so a skill reading adjacent data files is unprotected, and there is no grace flag for migrating an existing signed corpus (nothing is deployed, so none exists) |
| R6 | **Partial, deliberately deferred (2026-08-16).** `builtin_memory.go` does tokenised BM25-ish substring matching. The Raft-replicated inverted index, hybrid fusion and temporal decay are not in. Deferred because current performance is tolerable to ~100k records and an inverted index is a large architecture commitment for a personal store — revisit as the corpus approaches that. Note the open correctness issue tracked under R21, which is independent of the index |
| R7 | **Partial** — see the status note on the section itself |
| R0 | **Done.** `RaftNode.ApplyOrForward` + `NodeService.Propose`. Sessions, prefs, credentials, soul tune, channel state and memory writes forward from a follower to the leader; Dream, session pruning and the scheduler stay leader-gated singletons, and `Forget` stays leader-only on purpose. See the section for the two deviations from the design below |
| R2 | **Done.** Durable prompts in `BucketPrompts` with a CAS resolve, serialisable continuations, a leader-gated expiry sweeper, and all three scopes. `once` is the turn; `always` mints a revocable policy rule with `approval:` provenance; `session` is now replicated too — see below for why the in-process version was only half right |
| R3 | **Done.** Turn serialisation (all four queue modes, `internal/gateway/turnqueue.go`), the cluster-wide per-conversation lease (`internal/memory/session_lease.go`), and restart-safe delivery. The pending queue in the proposal was deliberately not built — the transports already provide the guarantee, and the real gap was duplicate processing on restart, now fixed by acknowledging the poll offset per update. See the section |
| R5 | **Partial.** The trust-contract half is done: `ContextEngine` returns `promptgen.ContextBlock`s rather than a rendered string, recall goes through `WrapContext` so `BuildSafety` covers it, and it is delivered as a user-role message immediately before the user's own — out of the system prompt, and positioned so the cached prefix survives. Ingest-time scanning landed too: `internal/promptguard` scans on episodic ingest and quarantines rather than drops, and recall skips quarantined records. Deviation: the marker is a `promptguard:<detector>` tag rather than `metadata["promptguard"]`, because `EpisodicRecord` carries no metadata map and a tag needs no schema change. `memory_write` is scanned too, and tool output and errors are routed through `promptguard.Redact` before the model sees them. SOUL load and skill-manifest load are scanned too, but they WARN rather than quarantine — a SOUL is the agent's identity and refusing it on a heuristic would take the assistant down over a false positive. `NeutraliseCloseTags` has no caller left now that recall is out of the system prompt, so it is wanted only if something system-bound reappears. R5 is otherwise complete |
| R20 | **20a/20b/20c done** (#21) plus the decrypt work the measurement turned up (#25). Latency −73% and allocation −99% geomean versus the starting point; allocation no longer scales with corpus size. **20d (the band prefilter) is open**, and its case is weaker than written — decrypt fell from ~71% to ~32% of a query, so the cosine arithmetic is now the largest share. The minimum score floor is also still open |
| R21 | **Not started, deferred with R6 (2026-08-16).** Embedding failures at ingest are still skipped silently, and the backfill `builtin_memory.go` reasons about still does not exist. Worth flagging that this half is a live correctness bug rather than a performance item: a record whose embedding failed is stored and never becomes semantically searchable, with no signal to anyone. Small and independent of the index when it is picked up |
| R13 | **Done.** Level-0 skill index (`internal/node/skill_index.go`), complete rather than ranked, filtered only to what this node can run; `MaxDescriptionChars` enforced at parse |
| R14 | **Mostly done.** Pinned blocks, caps, the failure budget and the terminal non-error give-up all shipped. Open: the Dream-acts-on-threshold half — `NeedsConsolidation` exists and nothing calls it |
| R15 | **Done.** `internal/memory/self_taught.go` + the similarity guard, pending refinements, bounded history with rollback, size limits, and the agent-tier capability floor |
| R16 | **Done.** `internal/compute/review.go`, with `ArtefactStore` as the fork's entire write surface — enforced structurally rather than as a policy scope |
| R17 | **Done.** `internal/memory/self_taught_curator.go`, leader-gated. See the section for the three things building it changed |
| R18 | **Half done.** The self-taught materialiser, the `prose` runtime, and `ScanAgent` shipped; tier-first precedence shipped ahead of the rest. Open: `BucketSkills` / `BucketSkillBlobs`, the importer/exporter, and verbatim manifest bytes so a detached signature survives a round trip |
| R8–R12 | Not started, except R19. R13–R18 (skills + self-learning) are untouched; R18 is a prerequisite for R15, and both are now documented as aide decisions (`lobslaw-skill-storage-model`, `lobslaw-skill-approval-lifecycle`) |

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

- [x] Restart mid-conversation; the next Telegram message continues with full context.
      Covered by `TestConversationLogPersistsAndReloads` (a fresh log IS a restarted process) and
      `TestRESTSessionSurvivesServerRestart`. Verified rather than assumed.
- [x] Two gateway nodes alternating on the same chat produce one coherent history.
      **The restart case is not this case.** Persistence was tested; ALTERNATION was not, and it
      is where the risk lives: each node keeps its own in-memory cache, so after B appends, A's
      `Load` could serve its stale cache and lose B's turn. A single hand-off would not expose
      that — the node reading has no cache of its own to prefer.

      Also tested: a node whose durable write failed while it was a follower must not then serve
      its thin cache in place of the fuller durable history once the store is reachable again, or
      a brief follower period leaves one node permanently behind.
- [x] A REST client passing `session_id` gets continuity; one that omits it gets a fresh session.
      Both halves already covered, plus isolation between sessions, scoping to an owner, and
      rejection of separators in the id.
- [x] A conversation past `max_messages` retains a summary of the dropped prefix, and the model
      demonstrably still knows a fact stated only in the dropped region.
      The trim and the summary were each tested in isolation. What was not: that `Load` returns
      the summary ALONGSIDE the trimmed messages, which is the only route by which a fact from
      the dropped region reaches the next turn — from there it travels as
      `ConversationSummary` into the prompt.

      A transcript that is only a summary is still a transcript. After a long idle period the
      recent messages can age out entirely, and falling through to the cache there would discard
      the one thing still remembering the conversation.

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

### The half that was still in-process

`session` scope was recorded on the prompt record and then honoured out of a `map[string]struct{}`
in `compute`. The comment defending that was not wrong, exactly — *"a grant that outlives what the
user was looking at is one they did not knowingly give"* — but it only ever covered the **restart**
axis. On the **cluster** axis it never held: same conversation, same continuity the user was
reasoning about, and they were re-prompted anyway because the next message happened to land on a
different node. That is not the continuity ending. That is routing.

So grants replicate, keyed `<session_id>\x00<action>\x00<resource>` in `BucketSessionGrants`, and
the bound the dying process used to supply becomes an explicit `expires_at`. **A process exiting is
a terrible TTL** — it made the lifetime of a security grant a function of deploy cadence, weeks on
a stable cluster and ninety seconds during a rollout, and neither of those is a decision anybody
made. Configurable via `security.session_grant_ttl`, default 24h, because the unit the user was
reasoning about is a conversation and conversations are a day-shaped thing.

Three things worth naming:

- **Expiry is enforced on read, not by the sweeper.** A grant revoked only when a background pass
  gets round to it is live for however long that pass is behind, and "how stale is the sweeper" must
  not be a question a permission check has an answer to. The sweeper is bucket hygiene.
- **A grant with no `expires_at` is treated as expired, not as eternal.** Every path that creates
  one writes the field, so a record without it is one this code did not write — and the safe reading
  of "I do not know when this stops" is that it already has.
- **The in-process map is kept alongside, not replaced.** A raft apply can fail and the user has
  already tapped the button; falling back to a local grant means the conversation they are in the
  middle of continues, degraded to what it was before rather than broken. It also means a node with
  no local raft keeps exactly its old behaviour rather than silently losing the feature.

`RevokeSession` closes the NOTE the in-process version left for whoever added a "forget this
conversation" command: a cleared conversation must not keep privileges the user believes they
revoked. Keyed by session id, it is now a prefix scan — and it removes expired grants too, so
"forget" is a statement about what is stored rather than about what is enforceable.

### Acceptance

- [x] Approve on node B a prompt issued by node A.
- [x] Approve after a full process restart; the turn resumes rather than asking the user to resend.
      (Telegram. REST holds the connection open and resumes in-request, so it stores no
      continuation — noted in docs/dev/GATEWAY.md.)
- [x] Concurrent resolves cluster-wide: exactly one winner, everyone else `ErrPromptResolved`.
- [x] `always` produces a visible, revocable policy rule (`lobslaw policy approvals` /
      `revoke-approvals`).
- [x] A `session` grant given on node A is honoured on node B, and survives a restart.
- [x] `always` cannot escalate past the hardline floor — the floor is evaluated before policy, so
      no allow rule can reach past it (`TestSessionGrantCannotReachTheFloor`).

---

## R2b — Telegram identity is a renameable handle

Found while wiring R2's `always` scope.

### Problem

`tgUserIdentity` returns `tg-@<username>` when the user has one, falling back to the numeric id
only when they don't. That string becomes `claims.UserID`, which is what policy subjects
(`user:tg-@alice`), role assignment (`cfg.Roles(userID)`), and memory scoping all key on.

Telegram usernames are changeable, and a freed handle can be claimed by somebody else. So:

- A rename orphans every rule, role, and permanent approval bound to the old handle. Fails safe,
  but silently — the user's grants stop working and nothing says why.
- Whoever claims the freed handle inherits all of them. **Fails open.**

This is not specific to approvals; it is how every Telegram principal is identified today. R2's
`always` button makes it more consequential (a permanent allow rule transferring to a different
person) but does not introduce it.

### Proposal

This turned out to be R7 gaps 1–2 rather than a Telegram change: if the gateway resolves to a
canonical principal, the handle stops being an identity at all.

**Shipped.** `identity.Resolver.ResolveChannel(ctx, channel, address, fallback)` resolves the raw
channel address — for Telegram the numeric id, which never changes — against the bindings an
operator declared under `[[user]].channels`. Telegram calls it at the edge, the only place that
still has the numeric id. Falls back to today's derived id when nothing is bound, so a deployment
that declares no bindings is unchanged and a new person can still talk to the bot without a config
edit.

An operator closes the hazard for a given person by declaring:

```toml
[[user]]
id = "alice"
channels = [{ type = "telegram", address = "123456789" }]
```

**Migration shipped, both paths.** Binding a person for the first time re-points them at a new
canonical id, and state written under the old one does not follow. An operator can either carry it
over or start clean:

```
lobslaw identity rebind tg-@alice alice          # dry run
lobslaw identity rebind tg-@alice alice --apply
```

Rewrites owners on vector and episodic records, commitments, scheduled tasks, prompts, session
`user_id`s, and policy rules subjected to that person — including approval-minted ones. Leaves
`role:` / `scope:` subjects alone (they name a group, not a person) and refuses to merge two
`user_prefs` records, reporting the conflict instead: prefs are keyed by the id itself, and
silently picking a winner between two timezones is worse than saying so.

Starting clean — bind, and let the person re-approve what comes up — is documented as the
legitimate simpler option in `docs/dev/GATEWAY.md`.

### Acceptance

- [x] A user who renames keeps their identity — once bound by numeric id.
- [x] Someone claiming a freed handle inherits nothing.
- [x] A deployment that binds nothing is byte-for-byte unchanged.
- [x] Existing `tg-@name`-keyed state is migrated (`lobslaw identity rebind`), and the re-grant
      path is documented for operators who would rather start clean.

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

- [x] Three messages sent during one in-flight turn produce one coherent history in arrival order.
- [x] `debounce` folds rapid-fire fragments into a single turn.
- [x] A node killed mid-turn releases its lease within the TTL and another node picks up the queue.
- [x] Queued messages survive a restart — by the transports, not by a queue. See below.

> **Partially shipped, 2026-08-15.** `internal/gateway/turnqueue.go` serialises
> turns per conversation, with all four modes, wired into both the Telegram and
> REST paths and configured by `gateway.queue_mode` / `queue_debounce`.
>
> One correction to the problem statement above: **polling mode was never
> affected.** `dispatchUpdate` is called from a plain loop, so updates were
> already serialised there. The race was webhook mode, where every update
> arrives on its own `net/http` goroutine, and the REST channel. The gate makes
> the transports agree rather than leaving correctness dependent on which one an
> operator picked.
>
> **Cluster half shipped 2026-08-15.** `internal/memory/session_lease.go`
> issues a raft-backed per-conversation lease over `LOG_OP_CLAIM`, with a TTL
> matching the responsiveness hard timeout and a heartbeat at a third of it.
> `TurnGate` takes it after local admission and drops it on release, so both
> channels get it without repeating the logic.
>
> The lease is its own bucket rather than fields on `SessionRecord`: it is
> written three times per turn (claim, heartbeats, release) against a
> transcript written once, and sharing a record would make every lease write
> contend with the append made by the turn holding it.
>
> Expiry is evaluated by the caller, not the FSM — it is wall-clock, and the
> FSM must replay identically on every replica. A node taking over a dead
> holder names that holder explicitly, so the CAS stays an exact comparison.
> This is the same split the scheduler uses.
>
> **`SessionRecord.pending` was not built, deliberately.** The proposal above
> assumes the queue is the only thing standing between a restart and a lost
> message. Checked against the transports, it is not:
>
> - **Poll** resumes from the persisted offset, so anything unacknowledged is
>   redelivered. The first-run flush only discards when there is no persisted
>   offset at all — a genuinely first run, not a restart.
> - **Webhook** writes its 200 only after the turn completes, so a restart
>   mid-turn leaves Telegram without a response and it retries. The dedup map is
>   in-memory, so after a restart the retry is not swallowed.
> - **REST** is synchronous; the client holds the retry, which is the right
>   place for it in a request/response API.
>
> A pending queue would be a second, weaker durability mechanism layered over
> those — and two mechanisms that disagree are worse than one.
>
> **The real exposure was the opposite one, and is now fixed.** Delivery is
> at-least-once with no *durable* dedup, and the poll offset was persisted once
> per batch, after every update in it had been dispatched. A crash after update
> 3 of 5 therefore left the offset covering none of them: on restart all five
> came back, the in-memory dedup map was empty, and the first three ran again —
> duplicate replies, duplicate tool calls, duplicate commitments. Losing a
> queued message is recoverable by asking again; silently acting twice is not.
>
> The offset is now acknowledged per update, after dispatch, so a restart
> replays at most the turn that was actually in flight. The poll loop also stops
> dispatching once its context is cancelled, rather than starting turns it is
> about to abandon.

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

### What was built, and where it differs from the proposal above

The fix is **effect-dependent** rather than a blanket deny, which is strictly better:

| Effect | On evaluation error |
|---|---|
| `deny` / `require_confirmation` | applied without evaluating |
| `allow` | skipped |

Skipping an erroring allow *is* fail-closed. Applying an allow yields the most permissive effect
there is, so whatever sits below it — a deny, a confirmation, or default-deny — is never a wider
grant. A blanket deny would turn one flaky evaluator into a total outage and buy no safety, since
the rules underneath evaluated cleanly on their own merits.
`TestSkippingAnErroringAllowIsNeverMorePermissive` asserts the property rather than leaving it as
an argument in a comment.

`condition_error_mode` was therefore **not built**. Its only purpose would be to restore the
blanket "skip", which reopens the deny leak this item exists to close, for a scenario — a flaky
custom evaluator — that cannot occur while no evaluator is registered at all. Raise it again if a
real one lands and turns out to be flaky.

**Found while doing this:** no condition evaluator is registered anywhere in production code, so
every conditioned rule is unevaluable today. An operator's time-of-day allow looks correct in a
listing, never grants, and says so only at warn level. `Engine.LogUnevaluableRules()` now reports
it at boot at error level, naming each rule, its unresolvable keys, and what it actually does.

### Acceptance

- [x] An erroring evaluator on a deny rule denies.
- [x] An erroring evaluator on an allow rule never widens the grant — asserted against a deny, a
      confirmation, and nothing beneath it.
- [x] Rules that can never behave as written are reported at boot, by id, with the consequence
      spelled out.
- [x] The `ARCHITECTURE.md` diagram matches the code and the TODO is gone.
- [~] `condition_error_mode` deliberately not built — see above.

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

- [x] Recall renders inside `<untrusted>` with a `memory:recall` source, in user position.
- [x] A record containing `</untrusted>` cannot escape its block on any path.
      **This one was false.** `WrapContext` wrote content verbatim between the tags, so a record
      containing `</untrusted>` closed its own block and everything after it read as instructions
      rather than as data.

      The ingest scanner quarantines records carrying a wrapper tag, which covers everything that
      went through ingest — and the box says ON ANY PATH. A record stored before the scanner
      existed, imported by another route, or content arriving from somewhere other than recall
      had no defence at all. The tags are neutralised at the render boundary now, which is the
      backstop that makes the claim true.

      Only the delimiter's opening bracket is escaped, and only where it begins one of these
      tags. Escaping every `<` would mangle any memory containing code — most of them here — and
      a defence that corrupts ordinary content is one somebody turns off.

      The scanner kept its own copy of the delimiter list. Two authorities for one fact: adding a
      wrapper tag would have left the scanner silently not covering it. `promptgen` owns the tags
      it writes; the scanner keeps only the chat-template control tokens, which nothing here
      emits.
- [x] A record containing zero-width characters or an "ignore previous instructions" phrase is
      quarantined at ingest and excluded from recall.
      Detection was tested. **Exclusion was not** — the only tests were of `IsQuarantined`
      itself, which proves the tag can be read and nothing about whether recall reads it. Recall
      is the path that replays a record into the SYSTEM PROMPT on every later turn, with no tool
      call in front of it and nowhere the user would see it; a record flagged at ingest and then
      recalled anyway is worse than one never flagged, because the flag is the reason nobody
      looked again.

      Tested both ways, because a guard that dropped everything would look identical to one that
      worked.
- [x] Prompt prefix stays stable across turns in a session (cache-hit assertion).
      One section was known to be deterministic; the WHOLE prefix across two turns was not
      checked. A section that changes between turns truncates the cacheable prefix at that point,
      so everything after it is re-billed in full on every turn of a conversation.
- [x] `BuildSafety` names every delimiter that can appear in a request. Test enumerates both sides.
      Both sides now come from one list, so "enumerates both sides" is a property rather than a
      pair of lists somebody keeps in step. A test also asserts the longer tags are listed first,
      or `<untrusted` would neutralise the front of `<untrusted-user` and leave a half-escaped
      tag nobody predicted.

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

`internal/memory/search_bench_test.go` and `sweep_test.go` (both new — this path had no benchmark of
any kind before). D=1536, sealed bbolt store.

**Where it started**, and why this section exists:

| N | latency | per record | allocated | retained after return |
|---|---|---|---|---|
| 1,000 | 16.3 ms | 16.3 µs | 13.2 MB | 6.7 MB |
| 10,000 | 145 ms | 14.5 µs | 132 MB | 66.9 MB |
| 100,000 | 1.59 s | 15.9 µs | 1.29 GB | **670 MB** |
| 1,000,000 | 19.2 s | 19.2 µs | 12.95 GB | **6.54 GB** |

Exactly linear, and `ContextEngine` called it passively on every turn. `search.go` claimed *"Fine for
personal scale (< ~100k records)"* — off by roughly three orders of magnitude in cost terms, and the
reason nobody looked. That comment is now deleted.

The retained column was the real ceiling: a top-3 result pinned ~6.7 KB per record in the whole
store, because truncating the candidate slice left its backing array — and every decoded record it
pointed at — alive. 670 MB held after a query at 100k records is an OOM on a small node, not a slow
query. No benchmark reports that, which is why `sweep_test.go` exists.

**Where it is now**, after 20a–20c and the AEAD work:

| N | latency | allocated | retained |
|---|---|---|---|
| 1,000 | **4.9 ms** | **127 KB** | ~0 |
| 10,000 | **48–50 ms** | **453 KB** | ~0 |
| 100,000 | **479 ms** | **3.5 MB** | ~0 |
| 1,000,000 | **5.17 s** | **34.6 MB** | ~0 |

Roughly **−73% latency and −99% allocation** (geomean, pinned, n=8, p=0.000), and allocation no
longer scales with corpus size. The usable envelope moved about 10×: comfortable to ~10,000 records,
tolerable to ~100,000. It is still O(N), so 1M is still 5 s — only 20d changes that.

### Where the time went, and where it goes now (N=10,000, D=1536)

| Layer | originally | share | now |
|---|---|---|---|
| decrypt only — `ForEach` with a no-op body | 88.8 ms | **~71%** | **15.2 ms (~32%)** |
| \+ `proto.Unmarshal` | 113.6 ms | ~91% | folded into the wire scan |
| cosine arithmetic alone, no I/O | 19.6 ms | ~16% | unchanged — now the largest share |
| └ of which the redundant `norm()` | 3.3 ms | ~3% | 0 (stored, 20a) |
| `sort.Slice` over all candidates vs top-3 | 1.3 ms | ~1% | 0 (bounded heap, 20c) |

**The cost was not the cosine.** The arithmetic the algorithm actually needs was its *smallest*
component; every query secretbox-decrypted and proto-decoded the entire corpus, including
`VectorRecord.Text`, which scoring never reads.

> **One prediction here was wrong, and the benchmark caught it.** 20b was written on the assumption
> that the text field dominated decode allocation. It did not: of the ~70 MB decode added, **61 MB
> was the embedding slice** (1536 floats × 4 B × 10,000), which `proto.Unmarshal` reallocates per
> record because `Reset()` drops capacity rather than keeping it. Text was ~6 MB. A narrow generated
> message with `DiscardUnknown` — the obvious reading of 20b — delivered −6.5% bytes and no
> measurable time change. The fix that worked was a `protowire` scan into a **reused** buffer.
> Anything written below about where cost lives should be re-measured before it is trusted.

**The remaining shares have inverted.** Decrypt is now ~32% rather than ~71%, so the cosine
arithmetic is the largest single component for the first time. That weakens 20d's payoff relative to
the original estimate and makes the arithmetic worth a look — the opposite of the conclusion this
section reached when it was written.

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

**20a, 20b and 20c are done** (PR #21), along with the decrypt work that the measurement above
turned up (PR #25) — see [Decrypt cost](#decrypt-cost--not-originally-a-numbered-item) below, which
was not a numbered item when this section was written. **20d is open**, with a weaker case than it
had.

### 20a · Store the norm — ✅ done (#21)

`norm(v.Embedding)` is a property of the stored vector, recomputed on every query for every record.
Add `float32 norm = 11` to `VectorRecord`, populate at write time, fall back to computing it when
absent so old records still work.

### 20b · Decode only what scoring reads — ✅ done (#21), but not the way this proposed

`proto.Unmarshal` decodes the whole record — `Text`, `metadata`, everything — and scoring reads only
`embedding`, `scope`, `retention`, `owner`, `visibility`. Two options, in preference order:

1. **Split the payload.** Keep the embedding plus filter fields in a narrow `VectorIndexEntry` in its
   own bucket, with the text and metadata in the existing record fetched only for the surviving
   top-K. Best win, and it composes with 20d.
2. Hand-rolled partial decode of just the needed field numbers. Cheaper to build, uglier, and it
   couples to field numbering.

### 20c · Bounded top-K instead of sort-everything — ✅ done (#21)

`vectorSearch` appends every passing record then `sort.Slice`s the lot for a top-3 result. A
`container/heap` of size K is O(N log K) time and O(K) memory. The time win is small; the allocation
win is not, and allocation is what is generating 138 MB of GC pressure per query.

### 20d · Prefilter that never touches the sealed payload — ⬜ open (the structural fix)

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

- ~~**Delete the "fine for personal scale (< ~100k records)" comment**~~ — ✅ done (#21).
- ~~**Surface dimension mismatch.**~~ — ✅ done (#21): `vectorSearch` counts skipped records and
  warns once per query with the query width. Uses `slog.Default()` because it is a free function;
  threading the node logger through is still open.
- **Add a minimum score floor** — ⬜ still open, and now the most valuable thing left in this
  section that is not 20d. Nothing anywhere applies one. Nothing anywhere applies one, so a query with no good match still
  injects the top-3 least-bad records into the prompt as *"Relevant context from prior
  conversations"*. Cosine 0.1 noise presented as relevant context is an effectiveness bug, not a
  performance one. `TestCosineAccumulationPrecision` notes the precision caveat that becomes relevant
  once an absolute threshold exists (float32 accumulator, measured rel. error 4.6e-07 at D=4096 —
  fine for a floor, worth knowing about).
- **Surface dimension mismatch.** `vectorSearch` silently skips records whose width differs from the
  query. Changing embedding model therefore makes the entire existing corpus invisible with no error,
  no warning and no metric. Count them and log once per query at WARN.

### Acceptance

- [x] Allocation per query is O(K), not O(N). — 135 MB → 453 KB at N=10,000.
- [x] A dimension-mismatched corpus produces a warning, not silence.
- [~] `BenchmarkVectorSearch/N=10000` improves by at least an order of magnitude. — got 3×
      (145 ms → 48 ms). The order of magnitude needs 20d; it was never achievable by decode tuning
      alone, which the layer breakdown above should have made obvious when this box was written.
- [ ] Band lookup performs no `crypto.Open` on non-candidate records — asserted, not assumed.
- [ ] Band keys are not raw content fingerprints.
- [ ] ANN recall vs exact scan is measured, with automatic fallback on degradation.
- [ ] A query with no good match returns nothing rather than the least-bad three.

### Decrypt cost — not originally a numbered item

✅ **done (#25).** It has a subsection rather than a number because it did not exist as a plan: the
layer breakdown above turned it up, and it turned out to be the largest single win available.

Two costs were in `crypto.Open`: Salsa20 has no hardware acceleration, and `Open(nil, ...)`
allocated a plaintext per record. Measured on one record's worth of payload, 30 interleaved samples
pinned to dedicated cores:

| AEAD | sec/op | throughput | alloc |
|---|---|---|---|
| nacl/secretbox, fresh buffer (was) | 9.08 µs | 708 MiB/s | 6.6 KB |
| nacl/secretbox, reused buffer | 6.93 µs | 928 MiB/s | 0 |
| XChaCha20-Poly1305, reused | 2.67 µs | 2.35 GiB/s | 0 |
| AES-256-GCM, reused | **1.09 µs** | **5.78 GiB/s** | 0 |

Shipped as a per-record versioned envelope with the AEAD chosen at runtime — AES-GCM where the CPU
accelerates AES, XChaCha20-Poly1305 otherwise, both always openable so mixed hardware interoperates.
Legacy secretbox values keep opening. See aide decision `lobslaw-encryption`.

Three things worth carrying forward:

- **Buffer reuse was worth 1.31× on CPU, not just allocation.** An earlier read of noisier data said
  "no CPU change"; that was wrong.
- **Benchmark interleaving is not optional on this hardware.** A single `-count=30` reported ±30%
  because Go runs all of one benchmark's samples before the next, and each saw a different frequency
  state under the `powersave` governor. Interleaved runs report ±1–13%. Every figure in this section
  was re-measured that way.
- **Whatever the fastest AEAD is depends on the machine, and the gap is large** (5.78 vs 2.35 GiB/s).
  Any future "just pick X" reasoning here should be re-measured on the target, not assumed.

---

## R21 — Embedding outbox

⬜ **Not started.** Re-verified 2026-08-15: `episodic_ingest.go` still skips silently, and the
`builtin_memory.go` comment still reasons about a backfill that does not exist. Unchanged by the
R20 work — that made the scan cheap, not the corpus complete.

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
- [x] A modified reference file fails verification. References may pin a `sha256`, verified at
      parse and re-verified before exec (same window argument as the handler). A **signed** manifest
      must pin every reference it declares — a signature covering the code and not the rules
      document that drives it reads as provenance while guaranteeing less than it appears to.
      Unpinned references stay legal under `SigningOff`: a skill with nothing to sign against
      should not have to carry digits it cannot verify.
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

- [x] A vision call whose primary provider 429s succeeds on the next configured provider.
- [x] A 400 / content-length error aborts immediately without burning the candidate list.
- [x] A provider returning 401 is demoted with a distinct, actionable log line.
      Required a fourth failure class: `credential-rejected` is neither transient (waiting does
      not fix a wrong key) nor permanent (the next provider has its own key). It reverses an
      earlier decision that 401 was permanent — see the note in `drivertest/conformance.go` for
      why that was defensible and why the ERROR-level log is what makes advancing safe now.
- [x] Health tracking: a provider that failed recently is skipped rather than re-tried every turn.
      Per-node, not Raft; no half-open probe state — the chain is the probe.
- [x] A chain step falls through without abandoning the chain.
      **Chains route, and their later steps run.** `Resolver.Resolve` is on the turn path: the resolved chain
      picks WHERE THE BACKUP WALK BEGINS, so everything the walk already did (the trust floor at
      every candidate, health cooldowns, failure classification, a span per attempt) applies
      unchanged and a chain decides its start.

      The signal that was missing: `min_complexity` and `domains` had no producer anywhere, so of
      the three trigger kinds only `always` could ever have fired. A preflight judge now supplies
      a complexity score, domain tags and a hint from the cheap `[compute.roles] preflight` model
      — the role already existed for exactly this shape of work. It is bounded and its failures
      are cheap: an unavailable or nonsensical preflight yields the neutral judgment and the turn
      routes on the default, because a turn must not die because the thing that decides HOW to
      route it was unavailable.

      **A found bug, worth recording.** When no chain matches, the resolver synthesises a
      fallback picking the highest-trust provider, breaking ties alphabetically. That answers
      "who could serve this at all" and is the wrong answer to "where should this start" — with
      two equal-trust providers it silently moved every unmatched turn off `roles.main` and onto
      whichever label sorted first. Routing now overrides the primary ONLY when a chain actually
      matched. Caught by a test asserting a complexity-5 greeting still starts at the primary.

      **The multi-step half.** A chain's later steps run as a PIPELINE: step N's output is
      rendered into step N+1's `prompt_template`, and the last step's output is the answer. A
      "reviewer" is then just a step whose template says "improve this" — the mechanism does not
      need to know what a reviewer is, which is why roles stay descriptive rather than becoming
      behaviour.

      **On the turn's answer, not on each round-trip.** Step 0 is the whole tool-call loop,
      however many round-trips that takes; the later steps run once, on what it finally said.
      Running a reviewer against an intermediate tool call would be reviewing a decision to look
      something up.

      Step-level fallthrough comes free: a step's provider becomes the start of the backup walk,
      so a rate-limited reviewer falls through to that provider's backups rather than failing the
      step. Steps go through `callLLM` rather than dispatching directly, so each is billed,
      hooked, budgeted, traced, floor-checked and failed over exactly as the main turn is — a
      second code path for provider calls is how one of them comes to be missing the trust floor.

      **A failing step returns the best answer so far.** By that point the user has a complete
      reply from step 0; losing it because a reviewer's provider was rate-limited would make a
      chain a liability rather than an improvement. The same applies to an empty reply (a failure
      that did not announce itself) and to a template the operator got wrong — silently
      substituting different instructions would produce a reply they did not ask for and cannot
      account for.

      Steps get NO TOOLS. A step refines what the previous one said; tools would reopen the whole
      tool-call loop once per step, and a chain of three would be three agents rather than one
      answer passed along.

      Episodic memory ingests the FINAL answer. Remembering step 0's draft would seed
      consolidation with a reply nobody saw, and the assistant would later recall having said
      something it did not say.

      *Previously recorded here:* `Resolver.Resolve` has no
      callers — verified exhaustively, `ResolveRequest` is constructed nowhere and
      `Node.Resolver()` is never read. The turn path is `ProviderRegistry.Chain`, which walks
      `backup` links and knows nothing about triggers, multi-step chains or per-chain trust floors.
      So `[[compute.chains]]` is parsed, validated for coherence, logged at boot, and inert.

      Building step-level fallthrough for a resolver nothing calls would be building on sand, so
      the first move was to stop the config lying: an operator with chains configured now gets a
      boot warning naming them and saying what routes instead. The resolver is kept for its
      validation — deleting it would silently start accepting broken chains before the routing
      lands.

      What remains is R8's actual premise: one selection pipeline, with `Resolver.Resolve` on the
      turn path and multi-step execution (reviewer handoff, prompt templates) in the agent. That
      execution half does not exist either, which is why this is L-sized rather than the small
      change the box implies.
- [x] Trust-tier floor is honoured at every candidate, not just the first.
      Enforced on the chat backup chain and on every modality chain — a vision provider is handed
      the user's image and a speak provider the text of the reply, so they are not lesser
      recipients of content.

      **Correction to the first version of this entry**, which claimed the floor was enforced
      nowhere and that `soul.ValidateProviderTier` had no callers. It had one:
      `wireLLMProviders` has validated every configured provider against the floor, fatally, since
      long before the runtime check. So the config-time hole described there did not exist — you
      could never boot with a below-floor provider configured. The claim came from a truncated
      grep read as a complete one.

      The real gap is narrower and still worth closing: **the soul is tunable at runtime.** An
      operator raising `min_trust_tier` after boot had already passed the boot check, so the change
      took effect in the system prompt and nowhere else — providers already in the registry carried
      on serving turns at the tier they were admitted at. The runtime check closes that, and is
      defence in depth otherwise. The duplicate boot check added alongside it has been removed:
      it warned for backups, implying a leniency the real check does not offer.

- [x] `hint = "deep"` resolves through a chain an operator can inspect and override.
      A hint selects the chain whose LABEL matches it, so `deep` means whatever the `deep` chain
      in the operator's config says it means, and editing that chain redefines the hint. Sugar
      over chains, not a second mechanism: a hint naming no chain falls through to ordinary
      trigger matching rather than inventing a route, and a hinted chain is held to the same
      trust floor as a triggered one.

      An explicit hint from a channel or API caller SKIPS the preflight call entirely — somebody
      who said "deep" has already answered the only question it was going to ask. An
      unrecognised hint, from the caller or from the model, is discarded rather than passed on:
      it would match no chain and route to the default, which is indistinguishable from having
      had no opinion.
- [x] Per-attempt audit entries (provider, latency, outcome, cost).
      Landed as R24's span model rather than as a separate mechanism — a per-attempt record and a
      turn trace are the same data, and building both would have produced two accounts of one turn
      that eventually disagree. Every candidate emits a span: the winner, the ones that failed and
      advanced, and the ones never tried (demoted by health, or refused by the trust floor). The
      skipped ones matter most — "the chain skipped three providers before succeeding" is the shape
      of a developing outage and is invisible if only attempts are recorded.

      **Found while doing it: `CostRecord` was hardcoded to `CostUSD: 0` with an empty provider
      label.** R24's problem statement says the cost is "computed and then discarded"; it was never
      computed. `dispatchWithBackup` did not return which provider won, so the caller had nothing to
      price against, and a `// Phase 5.4 will fill this in` comment had been standing in for it.
      Every turn to date reported a spend of nothing — which also means the budget's spend cap has
      never fired. The winning entry now comes back, carrying its model and pricing.

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

- [x] Every hardline pattern is refused with every policy rule set to allow and confirmations off.
- [x] A test asserts no config path disables the floor.
- [x] `~/.ssh/id_*` reads are refused; `~/.ssh/config` prompts.
- [x] Docs state plainly what the denylist does and does not protect against.

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

- [x] A slow REST turn streams interim progress where the client supports it
      (`Accept: text/event-stream`; a client that does not ask is byte-for-byte unchanged).
- [x] Responsiveness timers are tested once, against a fake `Responder`.
- [x] Adding a channel requires no new timer code.
- [x] **The hard timeout now reaches REST, which had no cap at all.**

**Found while doing this:** the hard timeout did not work anywhere. On expiry the agent produces a
graceful "this took too long" reply, and that call needs a fresh context — it used
`context.Background()`, so a provider that had stopped responding (the usual reason a turn hits its
cap) was re-entered with a context that could never cancel. Telegram's 90s cap was defeated the
same way. Now bounded by `AgentConfig.SummaryTimeout`, built with `context.WithoutCancel`.

The static fallback also told a timed-out user they had "hit my tool-call limit", sending them off
to narrow a request that was never too broad. The wording follows the reason now.

`Prompt` on the Responder is deliberately **not** built — see `docs/dev/GATEWAY.md`. Telegram and
REST render confirmations genuinely differently and both work; an interface over that, for a
channel that does not exist, would be a guess.

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

- [x] `lobslaw memory show | list | forget | share | unshare` — shipped earlier.
- [x] A consolidation log: what Dream merged, superseded or left alone, and why. The verdict and
      reason were already computed and then written to a log line and discarded; they are now a
      durable owner-scoped record, readable with `lobslaw memory consolidations`.
      Every verdict is kept, including `keep_distinct` — "why did it NOT merge these" is asked as
      often as the opposite and a log of changes cannot answer it. A decision that failed to apply
      is recorded as attempted, with the error, because that is exactly when somebody goes looking.
      Bounded at 90 days / 5000 entries, pruned by Dream itself.
- [x] Optional `memory.write_approval` (hermes's key name) staging agent-initiated writes for
      approval, reusing R2's prompt machinery.
      **Everything the agent writes is gated, not a guessed-at subset.** A boundary drawn around
      "facts about the user" has to be inferred, and inference gets it wrong in both directions.

      Built as a low-priority POLICY rule rather than a branch inside the tool, and that is what
      makes the answer reusable: "for this conversation" is a session grant, "always" mints a
      visible and revocable rule, and an operator wanting something narrower writes an ordinary
      rule that outranks the config default. A branch consulting a bool would have needed its own
      notion of "already approved" and grown a second, subtly different approval system beside R2.

      Two things this needed. `policy.Engine.SetDefaults` holds config-derived rules IN MEMORY —
      the rule bucket is raft-replicated operator intent, and every node writing its own copy at
      boot would turn a local setting into contested cluster state. And the gate uses its own
      action (`memory:write`), because reusing `tool:exec` would mean the allow rule that lets
      memory_write run at all silently satisfied it. `RequireApproval` does not take an action
      parameter, so that mistake is not expressible.

      The prompt carries the content being written. Unlike a trace span, the audience here is the
      person deciding — a confirmation that says only "the agent wants to write a memory" is one
      nobody can answer usefully, so they answer it reflexively, which is worse than not asking.
      A DENIAL carries no content: it is not a question, and the person seeing it is not deciding.

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

- [x] Index cost is O(skills) in *names*, independent of body size. References are named, never
      inlined.
- [x] Every installed skill appears at level 0 regardless of the user's message, and the rendered
      index tells the model the list is exhaustive.
- [x] A skill whose gating fails is absent from the index, and the drop is logged once with the
      reason — a skill vanishing silently is indistinguishable from one that failed to parse.
- [x] Over-long descriptions fail at parse with the offending manifest named. Counted in runes.
- [x] Levels 1 and 2 (`skill_view(name)` / `skill_view(name, path)`).
      The body concept landed first, as the entry said it must. A manifest may name a `body`
      document — `SKILL.md` by convention — and pin it with `body_sha256`.

      **Pinned for the same reason the handler is.** A signature over a manifest that merely NAMES
      a document leaves the document swappable, and the swap would not break the signature. A
      skill's instructions steer what the agent does as surely as its code does, so a signed
      manifest declaring a body without a digest is refused — the rule `handler_sha256` already
      followed.

      The digest is re-checked at READ time, not trusted from parse: the invoker re-hashes the
      handler immediately before exec for the same reason, and a document swapped after
      registration is exactly what the digest exists to catch. A mismatch is REFUSED rather than
      served with a warning, because the caveat would land in a log nobody reads and the
      instructions in the model's context.

      Only DECLARED paths are served. Otherwise the agent could read any file beside the manifest
      by naming it, which is a directory listing dressed as documentation — and a traversing path
      is refused even if a manifest declares it, because a manifest is not a licence to read the
      filesystem.

      **A skill with no body is ordinary, not an error.** Many are a handler and a description,
      and reporting that as a failure would teach the agent to avoid a tool that is working. A
      typo in the NAME reports differently, because it is a different problem with a different
      fix.

      Documents are bounded and the truncation is announced: a skill bundling a reference manual
      would otherwise fill the context window progressive disclosure exists to protect, and a
      document that stops mid-sentence otherwise reads as one that ends there. The cut lands on a
      rune boundary, or the model reads a replacement character as content.

**Found while doing this:** `promptgen.GenerateInput.Skills` existed and `BuildSkills` rendered it,
and **nothing ever populated it**. The "Installed Skills" section said "(none installed)" on every
turn no matter what was installed, so a skill could only be invoked by a model that guessed its
name. Level 0 did not exist at all; `AgentConfig.SkillsProvider` is what fills it.

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

- [x] Prompt prefix is byte-identical across turns within a session — a mid-session write is
      durable immediately and invisible to that session's prompt.
- [x] A write past the cap errors with current usage and leaves the store unchanged.
- [x] The consolidation threshold fires at 80%, before a write can fail.
- [x] N consecutive failures yield a terminal **non-error** result — an error invites another
      attempt — and stop touching the store; the user still gets a reply.
- [x] promptguard on write: these land in system position, and provenance of the store is not
      provenance of the content.
- [x] Dream acting on the threshold.
      `NeedsConsolidation` was a signal with nothing reading it. Its own doc says why it fires
      early — "consolidate BEFORE a write fails, so the pressure produces curation in the
      background rather than an error the user sees" — and nothing did the curating, so the
      pressure produced the error instead.

      **Background curation, not an approval flow**, which is what that doc describes. It follows
      the merge phase's shape exactly: propose to the Summarizer, act only on a result that is
      demonstrably safe, and do nothing at all when no Summarizer is wired — a Dream pass on a
      node with no LLM must not quietly rewrite the one memory the user authored by hand.

      **The refusals are what make it safe unattended.** A summariser asked to compact somebody's
      notes can return anything, and what it returns REPLACES memory no retrieval pass can
      reconstruct. Refused: an empty result (the user would have to remember what they had asked
      to be remembered), one that is not shorter (the assistant rewording their notes for no
      benefit, again every pass), one that is fewer entries but MORE characters (length is what
      the cap measures), and one still over the cap. A single-entry block is left alone entirely:
      shortening it is editing what the user wrote rather than deduplicating what they wrote
      twice.

      The rewrite goes through the same `mutate` path as every other write, so the promptguard
      scan applies — a consolidation arrives from a MODEL and lands in system position on every
      future turn.

      A refusal logs at ERROR, not WARN: it means the block is still near its cap *and* the thing
      meant to fix it produced something unusable, so the user is heading for the failure this
      exists to prevent.

Editing is by unique substring rather than by id, and an ambiguous fragment is refused rather than
guessed at — editing the wrong memory is worse than being told to be more specific.

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

- [x] With `mode = "off"`, no store is constructed. Asserted by calling `wireSelfTaught` and
      checking the field is nil — not by mocking a flag. An unrecognised mode is off too.
- [x] A self-taught skill never shadows a signed or operator skill of the same name. The
      tier-first winner rule shipped separately under R18; this item contributes the
      `BucketSelfTaught` source it arbitrates over.
- [x] An `agent`-tier skill declaring a new binary or MCP server fails to load, with the reason.
      Enforced at the parse entry point (`ParseAgentSkill`) **and** at `Registry.Put`, because a
      rule applied by one entry point is one a second entry point silently does not apply.
      Refused: `credentials`, `binaries`, `network`, `requires_binary`. Allowed: `storage`, which
      is scoped to mounts the operator already configured and whose absence would stop the agent
      writing a skill that reads a file. The refusal names every capability asked for, and says how
      an operator can grant them (adopt the skill into the operator tier).
- [x] `propose` mode artefacts are inert until approved — `Active()` excludes them, and the mode
      decides the initial state rather than the caller, so a caller cannot smuggle ACTIVE past it.
- [x] One command lists (`lobslaw learned list`) and one discards (`learned discard`) everything
      self-taught. Nothing deletes: archived artefacts stay readable with `--archived` and come
      back with `learned restore`, as proposals rather than active.
- [x] Usage counters aggregate in raft rather than in a per-node file, so they survive a leader
      change and every node computes staleness from the same data. Batched in-process and flushed,
      because paying consensus per `skill_view` is the obvious way to make this worse than the
      sidecar it replaces.

Pinned artefacts resist both archiving and `discard` — somebody who decided an artefact is worth
keeping should not have to defend it from the curator every fortnight.

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

- [x] Review fires after the reply, never before it — `Consider` is called once the response is
      assembled, and does not block.
- [x] Skill review triggers on a tool-heavy single turn; memory review on turn count, per
      conversation.
- [x] A fork cannot spawn a fork (`ProcessMessageRequest.IsReviewFork`).
- [x] Scheduler-originated turns spawn no review — a turn with no channel has no human in the loop.
- [x] Routing the review to a non-main provider switches replay to a digest, derived from
      `RoleMap.IsMain(RoleReview)` rather than configured separately.
- [~] The fork cannot write outside the self-taught namespace. Enforced **structurally** rather
      than by a policy scope: `ArtefactStore` is the fork's entire write surface and can only reach
      the self-taught store, so there is no broader handle to misuse. That is a stronger guarantee
      than a rule evaluated per call — but it means the restriction does NOT appear in the audit
      log as a decision, because no decision is made. Worth revisiting if the fork ever needs a
      second write target; at one target, a policy scope would be ceremony around an interface that
      already cannot do anything else.

Prompt carries the preference order (refine before add), the anti-sprawl naming rule, and the
three-way axis split — memory is state, skills are procedure, soul is disposition. hermes's action
bias is deliberately **not** adopted: it is what forced them to build a curator and a stale
lifecycle, and lobslaw has Dream to catch a quiet pass plus `propose` mode making every marginal
artefact cost somebody an approval.

An unparseable decision is treated as "nothing learned" rather than retried, and a refused proposal
is logged and dropped — asking again costs another replay to maybe produce a marginal artefact.

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

- [x] A skill unused past the threshold transitions to stale, then archives, and stays recoverable.
- [x] A pinned artefact never transitions.
- [x] Nothing outside the self-taught store is ever touched — asserted, not assumed.
- [x] Staleness is computed from cluster-wide usage.

### What building it changed

**STALE had to keep loading, and `Active()` had to widen to say so.** Marking an artefact as a
candidate for archiving and simultaneously taking it out of service makes the transition to
ARCHIVED a ratchet with no possible reprieve — the 60 days between the two thresholds would be a
window in which nothing could happen. STALE now loads, and a use inside the window returns it to
ACTIVE. Without that, "seasonal" is indistinguishable from "dead": a skill for the quarterly report
is idle for eleven weeks by nature.

**A lifecycle transition must not count as an edit.** `lastActivity` reads `updated_at`, so a stale
mark that touched it would reset the very clock deciding whether the artefact archives — nothing
would ever leave the live set. `setState` leaves `updated_at` alone, and there is a test whose
failure message says exactly this.

**Staleness is measured from approval, not creation.** The clock on "has anybody used this" cannot
start before it was possible to. A skill that sat three months in the proposal queue and was
approved yesterday is one day old.

**PROPOSED is now bounded, and it was the uncomfortable call.** Archiving a proposal nobody has
looked at converts "not reviewed yet" into a decision nobody made. But an unbounded inbox is not an
inbox: a queue of two hundred is one nobody will work through, at which point the review fork is
writing into something that functions as `/dev/null` and the operator has *lost* the thing rather
than deferred it. So `proposal_expiry_days` defaults to 30, negative disables it, and the archived
record carries `archived_reason = "unreviewed"` — distinct from anything somebody declined, because
an operator reading the archive needs to tell those two apart.

Three things made that tolerable rather than merely necessary: approval no longer requires stopping
the cluster (`lobslaw learned approve` over mTLS, live by default with `--offline` as the opt-out),
nothing is deleted, and the operator is now *told* the queue exists.

The notice rides out on a turn the user is already having — no push mechanism, no per-channel
addressing, no delivery guarantees to get wrong, and any channel that can send a reply can carry
it. `[self_learning.notify]` takes two allowlists, `channels` and `subjects`, and **both** must
match: channels decides where (adding Slack later is a string), subjects decides who, and that
cannot be inferred because nothing in a conversation says which participant can act on the queue. A
channel allowlist on its own would tell a group chat what the operator has pending.

Appended to the outbound text only, never to the transcript: a notice recorded as an assistant
message is one the model reads next turn and reasons about, at which point the agent is discussing
its own pending proposals with the user — and it is in the summary forever.

Leader-gated via `singleton.Run`, the opposite of the materialiser: a cache is per-node and a
lifecycle is not.

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

### Store-to-cache contract — ✅ **SHIPPED FOR THE SELF-TAUGHT HALF**

The materialiser is built and wired for `BucketSelfTaught`, at
`<data-dir>/skills-cache/<name>/<version>/`, reconciled on boot and once a minute on every node
(not leader-gated: every node serves turns, and the cache is derived state, so two nodes produce
byte-identical directories). `Registry.ScanAgent` reads it, tags everything `TierAgent`, and runs
it through the capability floor. Convergent rather than incremental — `rm -rf` the cache and
restart is complete recovery, which is the property that says the store is really the authority.

Two things the design did not anticipate:

- **A self-taught skill is prose, and every manifest required a handler.** That encoded an
  assumption that a skill is a program. Added `runtime: prose` — a skill with no code, whose body
  is the skill, delivered the way references already are. Refused symmetrically: a prose manifest
  may not name a handler or pin `handler_sha256`, every other runtime must still name one, and the
  invoker refuses a prose skill with its own error rather than "unsupported runtime".
- **A record name becomes a directory name.** Traversal is refused before any path is built from
  it, not validated afterwards by the parse that reads the result back.

**The operator/signed half is now built too.** `BucketSkills` holds one record per
`<name>@<version>`; `BucketSkillBlobs` holds content-addressed payloads, so two versions sharing a
reference document store it once and re-importing an unchanged bundle replicates nothing.

The manifest bytes are stored **verbatim**, and the test that matters signs a manifest with a
comment, an unusual key order and trailing whitespace — everything a parse-and-re-encode would
quietly tidy away — then imports, exports, and verifies the signature against the exported file.
Mutating the store to normalise the manifest fails it with exactly that message. A manifest that
happened to be already-normalised would have let the test pass while the property it exists for was
broken.

Three decisions worth recording:

- **Every file is stored, not a recognised subset.** A skill's behaviour can depend on any file it
  ships — a prompt template, a rules document — and an importer keeping only what it understood
  would materialise something different from the skill somebody tested.
- **Blobs are verified on read.** A payload whose bytes no longer hash to its key is corruption, and
  returning it would hand a modified handler to the interpreter with the digest still looking right.
- **Export re-checks its paths.** A record is replicated state that a compromised or buggy importer
  on another node could have written, so trusting its paths on the way out would turn a bad record
  into arbitrary file writes.

**The materialiser and registry now read from the store.** The cache gained two subtrees —
`agent/` and `imported/` — namespaced rather than flat because they are scanned by different code
with different authority: everything under `agent/` is tagged `TierAgent` and passed through the
capability floor, everything under `imported/` has its signature verified and its tier derived from
the result. One directory holding both would make the tier depend on which scanner reached it
first. (The layout change is free: the cache is disposable, and `rm -rf` plus restart is still
complete recovery.)

The manifest reaches disk **verbatim** here too, with the detached signature beside it, so the
registry's real `SigningRequire` path verifies against the file the interpreter will load. A
mutation that re-renders the manifest on the way out fails that test.

**Found while wiring it: `skills.signing_policy` and `skills.trusted_publishers` were parsed,
documented, validated — and read by nothing.** The mount watcher calls `Registry.Scan`, which is
`SigningOff`, so a deployment setting `signing_policy = "require"` got no verification at all. The
same shape as `min_trust_tier` before it was enforced. Both are now read, and a `require` policy
with no trusted publishers refuses to boot rather than failing per-skill at scan time, where it
reads as "every skill is broken" instead of "no publisher is trusted".

**The mount is now an import source, and `Registry.Watch` is deleted.** Drop a skill in the
directory and it is imported into the store, replicated, materialised and loaded — the directory
keeps its job, but what it feeds is the store, and the store is the only thing the registry loads
from. `Watch` registered mount skills directly, which was precisely the second authority R18 calls
self-inflicted; it is removed rather than left unused, because a method that quietly reintroduces
two authorities is the thing somebody calls because it looks like what they want.

The consequence an operator has to know: **deleting the file no longer removes the skill.** It is in
the store. Said at boot, and said again once per skill when a source directory disappears, naming
the command that does remove it.

No leader gate: importing is a raft write and writes forward to the leader, so any node holding the
mount can do it — which matters, because a mount is per-node storage and leader-gating would mean
only the leader's copy was ever read. Two nodes importing identical content converge, since the
record is skipped when it already matches and blobs are content-addressed.

Idempotency is by CONTENT, not by version. A skill edited in place without its version moving is
exactly what a drop-in directory is for during development, and a version check would make those
edits invisible.

**The dev source is built**, gated twice — `skills.dev_source` AND `LOBSLAW_DEV` — with the node
refusing to boot when the key is set and the marker is not, naming both. Either gate alone is easy
to leave behind: a config file gets copied to production wholesale, an environment variable gets set
in a shell profile and forgotten. Both at once is a coincidence somebody has to arrange.

`TierDev` outranks signed, which is the point: tier-first precedence means a version bump cannot
promote a skill past its provenance, so an operator needing to override a signed skill locally has
to be given a separate SOURCE rather than a way to game the order.

**R18 is closed.** It was marked closed once prematurely, on the strength of the CLI landing, and
reopened during the 2026-08-17 roadmap review because `skills rollback` was on the acceptance list
and did not exist. It exists now, and the three boxes describing unverified behaviour have tests.

*The reopening, kept because the shape of the mistake is worth remembering:*

The CLI landing was read as the last piece, and it was not: `skills rollback` is on the acceptance
list and does not exist. `lobslaw learned` has `rollback`; `lobslaw skills` has list, import, export
and remove. Three more boxes describe behaviour that is probably present and has never been
verified — a node serving the library from the log with no mount, a deleted cache restoring
byte-identically, and an oversized payload naming the offending path (the store's error does name
it, and the gRPC ceiling was raised so that error is what surfaces, but nothing tests it end to
end).

Two of the six ARE done and can be ticked on the strength of existing tests: the signed
`import → export → diff` round trip is `TestTheRoundTripOverTheServiceIsByteIdentical`, and
`SigningRequire` behaviour is covered by the unsigned-bundle and missing-handler-digest tests.

**The CLI itself:** `lobslaw skills list | import | export | remove`, live over
mTLS with no `--offline` form — importing writes to raft, and doing it against a stopped node would
produce a record the running cluster never sees, which is the same reason `learned approve` refuses
it.

The BYTES travel, not a path. The command runs on somebody's laptop and the cluster is elsewhere,
so a service taking a directory would be reading one that exists perfectly well on the wrong
machine. The gRPC message ceiling is raised above the bundle limit deliberately: a bundle at the
4 MiB cap would otherwise fail with "message too large" rather than the store's error naming the
offending file.

A bundle is **parsed through the real loader** before it is stored — written to a temp directory and
run through `ParseWithPolicy` — so an import is held to exactly the standard a load is. Verifying
the signature by hand would have admitted a signed manifest pinning no handler digest, which the
loader refuses, and the skill would replicate everywhere and fail to load everywhere.

Name and version come from the MANIFEST, not from flags. Both are already stated in the file, and
two sources for one fact eventually disagree. `--tier=agent` is refused: that tier means "the agent
wrote this", and letting a person claim it from a command line would make provenance something
anybody can assert rather than a fact about where a skill came from.

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

### Precedence must become tier-first — ✅ **SHIPPED AHEAD OF THE REST**

Landed on its own, because it stopped being theoretical the moment R15 and R16 shipped: an agent
can now author a skill, so "bump the version, win the name" is a live path rather than a future
one. It needs none of the open questions below answered.

Order is now **tier → version → directory**, with `signed > operator > agent`. A version bump
cannot promote a skill past its provenance.

Two things found while doing it:

- `preferSigned` only applied under `SigningPrefer`, so under `SigningOff` signing did not factor
  at all. Precedence now derives from the verified fact rather than the policy — a signature that
  was checked is a fact about provenance whatever the policy says to *do* about signatures. Under
  `SigningOff` nothing is verified, so nothing reaches the signed tier and the order is unchanged.
  The field is deleted rather than left set-and-unread.
- `TierAgent` as the zero value silently demoted every `Skill` built as a struct literal. The zero
  value is "underived" instead, so a hand-constructed skill derives its tier from how it arrived.

The escape hatch for an operator overriding a signed skill locally is the dev source below, **not**
a version bump — a rule that can be beaten by editing a number is not a rule.

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

### Open questions — DECIDED 2026-08-16

1. **Blob threshold: fail at import, naming the path.** 256 KiB per file and 1 MiB per skill,
   configurable. An oversized reference is rejected outright rather than split or accepted, because
   every Raft apply replicates to every node and lives in snapshots thereafter — one careless skill
   would bloat every node permanently, and the author is the only person positioned to fix it.

2. **A dev directory is development-only; a node refuses to start otherwise.** The marker is an
   environment variable (`LOBSLAW_DEV=1`) rather than a config field, deliberately: config files get
   committed and deployed, environment variables are per-machine. A dev directory declared in config
   on a machine with no marker is a boot failure naming both, not a warning somebody scrolls past.
   The point of the store being authoritative is that production runs what the cluster believes is
   installed, and a live-watched directory that always wins locally is precisely the thing that
   makes that untrue.

3. **`history_depth`, default 10, configurable, with GC.** Named for what it bounds rather than
   `keep_versions`, which does not say whether the active version counts. It counts *prior*
   versions; the active one is always kept and does not count toward the limit.

### Acceptance

- [x] A node with no storage mount and no skills directory serves the full skill library from the log.
- [x] Deleting the cache and restarting restores every skill byte-identically.
      Both were true and neither was tested. `materialise_stored_test.go` covered a signed skill
      still verifying on disk and a hand-edited cache being corrected; what it did not cover is
      the cache being GONE — a fresh container, a wiped volume — which is the case the operator
      actually hits. The fixture manifest is deliberately not already-normalised (a comment, an
      unusual key order, trailing whitespace), because those are exactly what a re-render tidies
      away and a byte comparison is what notices.
- [x] `import → export → diff` is empty for a signed skill, and the export still verifies.
      `TestTheRoundTripOverTheServiceIsByteIdentical` signs a manifest that is deliberately not
      already-normalised — a comment, an unusual key order, trailing whitespace — imports it,
      exports it, and checks the signature still verifies against the exported bytes.
- [x] `SigningRequired` behaviour is unchanged after the move.
      An unsigned bundle is refused under `SigningRequire`, and a signed manifest pinning no
      `handler_sha256` is refused too — because the import is parsed through `ParseWithPolicy`
      rather than signature-checked by hand.
- [x] A self-taught skill cannot win a name against a signed or operator skill at any version.
      Shipped ahead of the rest — see above.
- [x] `skills rollback` restores a prior version across the cluster.
      **A rollback is nothing more than activating a version already in the log.** Every version
      imported is still there, so going back to one is a matter of saying which — no bundle, no
      re-import, no bytes moved.

      NOT re-validated against the current signing policy. The record was parsed through the
      loader when it arrived, and re-parsing it on activation would refuse a skill that a
      tightened policy no longer admits — which is exactly the situation somebody rolling back is
      trying to escape.

      The new version is activated BEFORE the old ones are deactivated. The other order leaves a
      window with no version of the skill in force, and a node materialising in that window drops
      a working skill from disk.

      Rolling back to the version already in force succeeds and says so. An operator scripting a
      rollback should not have to special-case having already done it, and an error there would
      be indistinguishable from a rollback that failed.
- [x] An oversized payload fails at import, naming the path.
      The gRPC receive ceiling was raised above the bundle limit precisely so this error is the
      one that surfaces: at the default limits a bundle at the cap is exactly gRPC's own default,
      and the operator would get "message too large" — true, unactionable, and naming nothing.

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

---

## R22 — Provider / modality layer

Full design: [`docs/dev/PROVIDERS.md`](/dev/PROVIDERS). Decision:
`lobslaw-provider-modality`.

### Problem

The provider layer grew one modality at a time, and each one reinvented
the same two ideas under a different name.

**The driver concept exists four times.** `VisionFormat`
(openai/anthropic/gemini), `AudioFormat` (openai/openrouter),
`EmbeddingFormat` (openai/minimax), and `ProviderConfig.Format`. Four
spellings of "which wire protocol does this endpoint speak", none of
them shared.

**Failover exists once.** Chat has it via `ProviderConfig.Backup` and
`Registry.Chain`. Vision, audio, PDF and embeddings have a single
endpoint each and no fallback — if it is down, the capability is gone.
This is not an oversight so much as unfinished work:
`SelectByCapability`'s own doc comment says its ordered result exists
*"for the future fallback-chain layer that will try each in turn on
transient failures."*

**Configuration is scattered.** `[[compute.providers]]` plus separate
config structs for vision, audio, PDF and embeddings, each with its own
endpoint, key and format.

And there is no generation of any kind — no speech, image or video.
Adding one today means a fifth format enum and a fifth config block.

### Proposal

Separate **drivers** (compiled-in wire protocols) from **providers**
(configured instances), collapse the config into one `[[provider]]`
table keyed by modality, and generalise failover to every modality.

Modalities: `chat`, `embedding`, `vision`, `transcribe`, `document`,
`speak`, `image`, `video`. `chat` and `embedding` are infrastructure;
every other modality is surfaced to the model as a **tool**, which is
the pattern `read_image` / `read_audio` / `read_pdf` already use.

Keeping them as tools is the load-bearing choice — it means any text
model works, `require_confirmation` already gates `generate_video`,
`TurnBudget` already counts them, and an unwired modality already
degrades honestly instead of the model pretending. See the doc for why
teaching the chat driver about multimodal content parts was rejected.

"Custom variants such as Qwen Cloud" need no new driver: they are
providers using the `openai` driver at a different endpoint, which is
already how `LLMClient` works.

### The abstraction

Three rounds of research moved this design three times, and each time
the thing that moved was one of four axes: interaction shape, billing
unit, credential kind, artifact delivery. They will keep moving. So the
interface makes those the only things a driver states, and everything
above it blind to them — full definitions in
[PROVIDERS](/dev/PROVIDERS#the-interface).

The load-bearing choices:

- **`JobHandle` is opaque and serialisable.** It holds an ARN, an
  operation resource name or a task id without the layer above
  knowing which, and it survives a round-trip through raft because the
  poll may happen on another node after a takeover.
- **Polling is a driver method**, and the driver states its own
  interval. Nothing constructs a task URL.
- **`Usage` carries a unit**, so a per-second-of-video call cannot
  report zero.
- **`Credential` is `Apply(ctx, *http.Request) error`** — the narrowest
  waist that hides OAuth refresh and SigV4 signing alike.
- **Artifacts normalise** to a path in a storage mount, whichever of
  the three delivery modes produced them.

### Adding one

Target: one file and one registry line. Everything cross-cutting —
retries, failover, budget, policy, tracing, artifact resolution,
credential refresh — lives above the driver, so a driver does not know
failover exists.

The accelerator is a shared **conformance suite** every driver must
pass: handle survives serialisation, failures are classified into the
three failover classes, usage carries a unit, expiring artifacts are
fetched in time, cancellation is honoured. Mocks by default, live
endpoints when credentials are present. It is what makes the eighth
driver a known quantity, and what catches a vendor changing shape.

**External drivers are one more in-code driver** whose methods shell
out to a sandboxed skill. Not a parallel path — same interface, same
conformance suite, same failover. A separate external path would drift
from the in-code one and the drift would be found by an operator
rather than a test.

### Sequencing

Ordering principle: **prove the abstraction against what already works
before betting new work on it.**

1. The waist — interfaces, `Usage`, `Credential`, `Artifact`,
   `JobHandle`, conformance suite. No behaviour change.
2. Mock driver for every modality, passing conformance.
3. **Migrate the four existing modalities onto it**, replacing the four
   format enums. Behaviour-preserving, and the checkpoint: if the
   interface cannot express what already ships, it changes here while
   that is still cheap.
4. The single `[[provider]]` table, existing config shimmed.
5. The no-network end-to-end harness — a full node on mock drivers
   serving a real turn.
6. Per-modality failover, three classes.
7. Async job plumbing: `JobDriver`, artifact resolution, commitment-
   backed poll handler.
8. New modalities: `speak`, then `image`, then `video`.
9. External drivers — REVISED, and deliberately unscheduled. See
   `docs/dev/PROVIDERS.md` → "Revised: declarative first, code
   second". A declarative template driver (endpoint, auth style,
   request/response field paths in TOML) covers the long tail of
   OpenAI-shaped clones with no code execution; skills remain the
   answer only for a genuinely different protocol, and only for the
   asynchronous modalities. Build either when a vendor appears that
   the layer below it cannot reach — a second vendor per modality is
   worth more today than an extension point nobody is extending.

Steps 1–5 add no capability, and that is the point: they make step 8 a
day per modality rather than a fresh argument each time.

### Acceptance

**Status (reviewed 2026-08-17): partly shipped.** The MODALITIES landed — speak, image and video
generate, and generated files are delivered to the user through an explicit artifact mount. The
DRIVER CONSOLIDATION did not: `mimeForAudioFormat` and the `PDFFormat` constants are still
per-modality, and there is no `driver =` key on a provider. So the boxes below that describe one
`Driver` type remain genuinely open, while the ones describing generation are done in substance
under a different shape.

This entry said ⬜ while three modalities were shipping, which is how a roadmap stops being read.
Reconciling the boxes against what exists is worth doing before R26 builds a second vendor on top
of them.

- [x] One `Driver` type; no `VisionFormat` / `AudioFormat` /
      `EmbeddingFormat` remain.
      **Done.** No wire-protocol format enum survives. `read_image` switched on a
      `VisionFormat` in two places — once to build the request, once to decode the reply — with
      three vendors inlined in each, so `driver = "anthropic"` selected the chat wire shape and
      said nothing about the vision one. It now resolves a `VisionDriver` from the `DriverSet`
      like chat, speech, image and jobs already did.

      **Read literally, "one `Driver` type" would mean collapsing the per-modality maps into one
      `map[string]Driver` with runtime type assertions.** That was not done, deliberately: the
      separate maps are what let a speech-only vendor simply be absent from the chat map, and
      replacing compile-time separation with an assertion is a downgrade. The box is read as one
      PATTERN — a named driver per (vendor, modality), resolved from a set — which is what the
      rest of the entry describes.

      Two things fell out of the move. Anthropic's `anthropic-version` header is a PROTOCOL
      version, not a credential, and a driver that left it to the wiring layer would work only
      for whoever remembered to set it — so drivers can now pass their own required headers.
      Google authenticates on the URL rather than in a header, which the old code handled by
      appending `?key=` to the endpoint before the request was built; a `QueryCredential` puts
      that behind the same interface as every other provider's auth, so the next such vendor does
      not grow another special case.

      Audio followed, with one difference worth recording: its driver is picked by the matched
      CAPABILITY rather than by the provider's `driver` key, because a chain can legitimately mix
      a Whisper endpoint with a chat-multimodal one. Vision's vendors differ by vendor; audio's
      differ by which capability they advertise.

      PDF turned out never to have had a format enum — its comment merely anticipated one, and
      now points at the driver seam instead so the next vendor does not reintroduce the switch
      that has twice been taken out.

      Embeddings was the worst of them: `EmbeddingFormat` threaded through the client struct and
      switched on at FIVE sites, with no tests at all. The client keeps what is genuinely its own
      — the endpoint suffix rule, the egress-scoped HTTP client, the declared dimension, and the
      re-projection of vectors around filtered-out empty inputs — and the drivers own the bytes.
      It takes a FACTORY rather than a built driver, because the suffix rule runs first and the
      driver needs the normalised endpoint.

      The single and batch paths stayed separate methods rather than collapsing into one: OpenAI
      sends `input` as a bare string for one text and an array for many, and unifying them would
      have changed the bytes every existing deployment sends to save a method.

      Writing the missing tests turned up the sharpest thing in the whole conversion. **The
      OpenAI batch response is placed by its `index` field, not by arrival order** — the API does
      not promise the array comes back sorted, and a batch reassembled in arrival order attaches
      every memory to the wrong text. Nothing downstream can detect that, because a vector is a
      plausible vector whichever text it came from.

      `mimeForAudioFormat` in `speak.go` survives and should: it maps an output CONTAINER to a
      MIME type (`wav` → `audio/wav`), which is not wire-protocol dispatch.
- [x] A node boots from a config whose every provider is `driver =
      "mock"`, with no network access, and serves a full turn.
      Chat, vision, audio, embeddings and jobs already had a mock; SPEECH AND IMAGE DID NOT, so
      such a node booted and then failed the moment somebody asked it to say something out loud.

      This is not only a testing convenience. It is the cheapest check that the driver seam is
      complete: a modality with no mock is one whose wire shape has not really been separated
      from its plumbing, because if it had been, substituting the wire would be trivial.

      Every mock produces PLAUSIBLE output — a valid RIFF/WAVE container, a PNG that decodes —
      rather than empty bytes. The artifact path forwards these to a channel, and Telegram
      rejects a voice note whose container it cannot parse, so a mock producing garbage would
      make the delivery path untestable exactly where it is fiddliest.
- [x] A vision provider whose primary returns 503 falls through to its
      backup within the same turn, and the fallthrough is logged.
      The behaviour was already there and tested per failure class. The LOG was not tested, and
      it is half the criterion: "the chain worked" and "the chain worked on its third try because
      two providers are down" look identical to an operator unless the fallthrough says so. The
      line names the provider that failed, not merely that one did — and a chain that succeeds
      first time logs nothing, or the signal would appear on every turn and mean nothing.
- [x] A provider declaring a modality its model does not support is
      warned about at boot via the existing models.dev capability data.
      **WARNED, NOT REFUSED.** The catalogue is third-party data that can be stale or wrong, and a
      self-hosted model may genuinely do something models.dev has never heard of. Refusing to boot
      on somebody else's data about somebody else's model would take a cluster down over a missing
      entry.

      The difficulty was not detecting it — it was not crying wolf, because a warning that is
      often wrong is one nobody reads, and the surrounding warnings then lose their value too.
      Three rules keep it quiet:

      - It speaks only about the five capabilities the catalogue HAS data for (chat, vision,
        audio-multimodal, pdf, function-calling). Speech, image, video, embeddings and
        transcription have no signal there, and warning about them would tell an operator their
        text-to-speech provider cannot speak.
      - It takes the UNION across catalogue entries, not the intersection. Two listings of one
        model name can disagree, and one of them claiming a capability is enough — the
        intersection would fire whenever any two entries differed.
      - An unknown model produces nothing. Treating "never heard of it" as "cannot do it" would
        warn loudest about the self-hosted deployments least likely to be in a public catalogue.

      It checks EVERY provider, not only those that opted into discovery — the operator most
      likely to have got a capability list wrong is the one who typed it by hand. It does not
      fetch the catalogue on its own account, though: a mandatory boot-time HTTP call would break
      an air-gapped node to deliver a warning.
- [x] `generate_image` can be gated by `effect =
      "require_confirmation"` with no new machinery.
      True already, and now checked. A builtin reaches the executor's policy gate as action
      `tool:exec` with its own name as the resource, so an ordinary rule covers it — the same gate
      subprocess tools and skills pass through.

      "No new machinery" is only worth claiming if something holds it to that, because it is
      exactly the sort of property that stays true until a dispatch path is added which skips the
      gate. A mutation removing the gate fails two of these tests.

      **The sharp part is WHEN.** The check fires before the provider is called, so a confirmation
      asks about a decision rather than about a bill — an image already generated has already been
      paid for. And a deny stays distinguishable from a confirmation: one is "ask me", the other
      is "never", and offering a choice that does not exist is worse than refusing plainly.

      Gating one modality leaves the others alone. An operator worried about image spend has said
      nothing about speech.
- [x] A `generate_video` call submits a job, returns from the turn
      immediately, and delivers the artifact later via a commitment —
      without holding the session lease or tripping the 90s
      responsiveness timeout.
- [x] A provider billed per second of video reports a non-zero cost.
      Zero is the current answer and it is wrong.
      It was worse than zero: generation reported NOTHING — not an approximation, not a zero with
      a note, but no cost record at all. The spend cap could not fire on a generated minute of
      video and the trace could not show it.

      `Usage` carries an optional unit and quantity, and `ProviderPricing` a `unit_usd` map. A MAP
      rather than a field per concept, because the set is open — video seconds, images, audio
      characters, opaque credits — and a field per vendor idea means a code change and a release
      every time somebody meters something new. Tokens stay as their own fields: they are the
      common case, they need the cached/uncached distinction a flat map cannot express, and
      moving them would make every chat turn pay a lookup to price the thing it always prices.

      **Added to the token cost, not substituted for it.** A reply that generates a picture costs
      its tokens AND its image.

      **An unpriced unit costs nothing and is not an error.** A plan-billed provider has no
      marginal rate, and refusing to account for the turn because nobody wrote down a price would
      stop the turn rather than the billing. The QUANTITY is recorded either way, so consumption
      stays visible where the marginal cost is genuinely nil — which is also most of what the
      plan-billed box below asks for.

      Costs reach the budget through a collector on the context, following `ArtifactCollector`. A
      builtin has no reference to the `TurnBudget` and should not: it would then be able to
      refuse a turn on the budget's behalf, halfway through, from inside a tool. It is DRAINED
      rather than read, because a turn generating two videos across two round-trips must not bill
      the first one twice.

      **One model, not two.** `ModalUsage` already existed for exactly this — its own doc saying
      it would "eventually absorb" the token-only `Usage` — and a parallel `Unit`/`Quantity` was
      added to `Usage` before that was noticed. The duplicate was removed and `CostRecord` now
      carries `ModalUsage`, with the token breakdown nested. Two accounts of what a turn cost
      eventually disagree about the answer.

      That also brought `BillingPlan` into the pricing path, which is most of the plan-billed box
      below: a prepaid plan costs nothing per call, and pricing it as though it did would inflate
      every turn that provider served. The QUANTITY still travels, because consumption against a
      quota is the meaningful number there.

      **Video is priced at the poll handler**, because that is where the cost is first knowable —
      a video is billed by the seconds actually produced, and the driver only learns that when
      the job completes. The provider label travels on the commitment so the rate card can be
      found again minutes later.

      It does NOT go to a TurnBudget. The budget it would charge is closed, and re-opening it
      would let a background job push a finished conversation over a cap it never hit.
- [x] A plan-billed provider whose quota is exhausted falls through to
      its backup and warns; it is not retried until reset, and it is
      not treated as a request error.
- [x] The job handle is opaque to everything above the driver: a
      driver returning an ARN, an operation resource name or a bare
      task id all work without the scheduler knowing which.
- [x] A provider requiring short-lived OAuth tokens (Vertex) and one
      requiring per-request signing (Bedrock) are both configurable
      without a static `api_key`.
      **The OAuth half is done.** `RefreshingCredential` caches a token behind a `TokenSource` and
      renews it — with a refresh margin, because a token that expires mid-flight fails a call that
      was valid when it started and the failure reads as an auth problem rather than a timing one.

      The lock is held ACROSS the refresh, deliberately. Concurrent turns would otherwise each
      mint a token: a stampede against a rate-limited endpoint, with all but one discarded.

      A failed refresh does NOT fall back to the stale token. A refresh that failed says nothing
      about whether the old token still works, and presenting an expired one produces a 401 that
      reads as a misconfigured key rather than a token-endpoint outage. An unknown expiry is
      treated as short rather than forever, or a dead token would be presented indefinitely.

      **The signing half is done too.** SigV4 is written out rather than imported: the AWS SDK
      brings a large dependency tree for one function, and this is a published, frozen algorithm.
      It reaches the body through `GetBody` and puts it back — a body consumed to hash it and not
      restored is signed correctly and then sent empty, and the server's complaint is "signature
      mismatch" rather than "your body vanished".

      **The tests do NOT assert AWS's published signature constants.** Writing one down from
      memory risks a wrong expected value that then gets "fixed" to match the implementation,
      which is worse than no test: it looks verified and proves nothing. They check instead that
      every component the specification covers actually changes the signature — body, method,
      path, query, host, region, service, date, secret, session token — that the header block is
      lowercased and sorted as described, and that the body survives.

      **So interop with a real AWS endpoint is UNPROVEN.** It should be confirmed against a live
      Bedrock call, or against the published vectors copied in verbatim from the documentation,
      before this is relied on. Recorded here rather than left implied.
- [x] An artifact delivered to an operator-owned bucket lands in a
      lobslaw storage mount, with no download step.
      The mount kind returns the provider's own path untouched, and the test asserts nothing was
      written into the default mount — which is what "no download step" means in practice.
- [x] An artifact delivered as an expiring vendor URL is fetched
      before it expires, and a poll handler that has not run is not
      silently dropped.
      The expiry half was already covered — the check happens BEFORE the request, so an expired
      URL reports what actually went wrong rather than surfacing as a puzzling 403 from a CDN.

      **The second half had no tests at all**: `wire_generation.go` was entirely uncovered. Every
      branch of that handler decides between RETRY (the job is still out there), GIVE UP LOUDLY
      (say so and stop) and CLOSE (nothing can ever poll this again), and getting one wrong either
      loses work that is already running and already being billed, or polls a dead handle until
      the deadline.

      Now tested: a running job asks to be polled again at the DRIVER's cadence; a transient poll
      failure retries, because a 503 from a status endpoint says nothing about the job; a
      permanent one closes; an undecodable handle is not retried forever; a missing driver is
      named, which is the restart case; and an expired job gives up WITH a notification, because
      a job that silently stops being polled is precisely what "silently dropped" means.

---

## R23 — External drivers as skills

Full design: [`docs/dev/PROVIDERS.md`](/dev/PROVIDERS). Decision:
`lobslaw-external-drivers`.

### Problem

An operator wanting a provider lobslaw has never heard of must write Go
and rebuild. That is the wrong bar for something that is, in most
cases, thirty lines of HTTP.

### Proposal

A skill manifest may declare `provides.modality`. Such a skill is
registered as a provider for that modality rather than as a bare tool,
and invoked with the modality's JSON request shape on stdin/stdout — so
any language works.

No new security boundary is introduced, because skills already have the
one this needs: signed manifests with a pinned handler digest (R19), a
Landlock/seccomp sandbox, optional netns isolation, an egress proxy
with a per-role allowlist, credential injection, and policy gating.

MCP was rejected — no signing, no sandbox, and a driver holds provider
credentials. Go plugins were rejected — in-process, so no boundary at
all. A bespoke driver protocol was rejected — it would duplicate four
things that are already right.

Offered for the tool modalities only. A subprocess spawn per invocation
is irrelevant for generation and unacceptable for `chat`.

### Acceptance

- [ ] A Python skill declaring `provides.modality: image` serves
      `generate_image` with no Go changes.
- [ ] It runs under the same sandbox, egress allowlist and signature
      policy as any other skill.
- [ ] Declaring `provides.modality: chat` is rejected at load with a
      clear reason.
- [ ] An unsigned external driver is refused under `signing_policy =
      "require"`, like any other skill.

---

## R24 — Turn trace export

Full design: [`docs/dev/TRACE.md`](/dev/TRACE). Decision:
`lobslaw-turn-trace`.

### Problem

Three questions an operator cannot answer:

1. *Why did that turn take 40 seconds?* There is no per-span timing,
   and the non-LLM work — retrieval, compaction, ingest — is entirely
   invisible.
2. *What is costing me money?* `CostRecord{ProviderLabel, Model, Usage,
   CostUSD}` is computed per LLM round-trip and then **discarded** into
   `TurnBudget` totals. `ToolInvocation` has no timing at all.
3. *Is my primary provider being used?* Failover walks silently.

### Proposal

A turn emits a tree of spans — `llm_call`, `tool_call`, `embedding`,
`retrieval`, `compaction`, `ingest` — with parent links, timings, usage
and cost, exported to any combination of OpenTelemetry, a
newline-delimited JSON file, and a webhook.

**Exported, not stored.** No raft bucket and no reporting command: a
trace is high-volume, short-lived, and neither agreed-upon nor state.
lobslaw is a harness; this is telemetry for whatever the operator
already runs.

The one genuinely new calculation is **tool context attribution**, and
the obvious version is wrong. A tool's cost is not the call that ran
it — it is the tokens its output contributed to every *subsequent*
prompt in the turn. A tool returning 8k tokens on the first of six
model calls is carried in five later prompts, so it costs roughly 40k
prompt tokens. That is usually the dominant cost in an agentic turn and
is currently attributable to nothing.

Kept separate from the hash-chained audit log: an audit entry that may
be dropped under load is not an audit entry, and a trace that must
never be dropped becomes a reliability problem on the reply path.

### Acceptance

- [x] Off by default. Absence, not a flag: with tracing off the recorder is nil, and a nil recorder is usable, so no instrumented path branches on whether tracing exists.
- [x] A turn's spans nest correctly in an OTel backend, with tokens and cost as attributes.
      Written against the OTLP wire format rather than the OTel SDK: the SDK brings a tracer
      provider, span processor, batcher and propagation layer, every one of which this package
      already has, so adopting it would mean converting our spans into its spans so its batcher
      could hand them to its exporter. Trace and span ids are HASHED from the turn and span ids
      rather than generated, so a turn groups into one trace on every node and every re-export.
- [x] No span carries message text, tool arguments or tool output. Asserted against the SERIALISED bytes rather than the struct, because the bytes are what leaves the process.
- [x] A collector that hangs neither slows nor fails a turn; dropped spans are counted. Record
      never blocks — a full buffer drops and counts. A trace with a hole that says "4 dropped" is
      usable; one that silently omits the interesting span is worse than no trace, because it is
      read as evidence of absence.
- [x] Tool attribution reflects re-sent context, not just the producing call — verifiable on a
      scripted multi-tool turn.
      A `context_carry` span per tool, parented to its `tool_call`, carrying the tokens the result
      contributed to every LATER prompt and the number of prompts it rode in. Its own kind rather
      than an attribute on the tool span, because it is a different event at a different time: the
      tool ran once, and the cost accrued across every prompt afterwards. Folding them together
      would make a tool look expensive to RUN when what was expensive was carrying its output.

      Flushed from a defer on every exit path — normal, budget-exceeded, confirmation, hard
      timeout, loop exhausted — because a turn that ended unusually is the one whose cost somebody
      is asking about. The `tool_call` span itself is emitted immediately rather than buffered, so
      a turn that dies still has one.

      One part is genuinely an estimate and is labelled as such: providers report usage for the
      prompt as a whole and never per message, so the share belonging to one tool result is
      approximated with the same chars/4 heuristic the context budget uses. Being consistently
      wrong with the budget is worth more than being differently wrong from it.

      `lobslaw trace` states it as a SHARE of the turn rather than adding it: the carried tokens
      were already billed on the llm_call spans, and summing both would make the one command whose
      job is "why did this cost what it did" answer it wrongly.
- [x] Cached tokens are priced as cached, not fresh.
      Already true and already tested — `EstimateCost` bills `CachedTokens` at `CachedUSDPer1K`,
      the Anthropic driver reports them, and the span records them separately. Verified rather
      than assumed.
- [x] A span for a non-token-billed call carries its own unit and
      quantity (`video_seconds`, `images`, `credits`…), not a token
      count of zero.
      `trace.Span` HAD the `Unit` and `Quantity` fields, with a comment explaining exactly why
      they matter — "a zero token count on a call that cost real money reads as a free call" —
      and nothing set them. Generation emitted no spans at all, so a turn that rendered a video
      showed a trace with a gap where the expensive part happened.

      One call now writes both the cost record and the span. The budget and the trace are two
      accounts of the same spend, and two call sites per modality is how they come to disagree.

      A FAILED generation emits its span but charges nothing: the provider did not deliver, and
      billing for it would make an outage look like usage — while a failure is exactly what
      somebody reading a trace is looking for.

      **Video is the exception, and deliberately.** Its job outlives the turn, so by completion
      there is no turn trace to attach to; the cost is logged at the poll handler instead. A span
      invented for a turn that ended minutes ago would be worse than none.
- [x] Plan-billed calls record quota consumed and are marked as such,
      rather than reporting a marginal spend of zero.
      `BilledTo` on the span. Without it a plan-billed call is indistinguishable from a free one:
      both report zero, and only one of them consumed something finite. An operator asking why
      their quota ran out would find a trace full of calls that apparently cost nothing.

      The QUANTITY is what carries the meaning there, so it travels even though the cost does
      not.

---

## Annex — provider API survey (2026-08-15)

Verified against vendor documentation rather than recalled, because
the assumption this survey overturned — "a custom variant is the same
driver at a different endpoint" — was itself a confident recollection.

**Interaction shape.** Alibaba Wan text-to-video is asynchronous:
`POST …/video-synthesis` with `X-DashScope-Async: enable` returns a
`task_id`; the caller polls `GET /api/v1/tasks/{task_id}` through
`PENDING → RUNNING → SUCCEEDED | FAILED` at roughly 15-second
intervals. Image-to-video runs **1–5 minutes**. Both the task id and
the returned `video_url` expire after 24 hours. OpenAI's image API, by
contrast, returns the image in the response — so both shapes are real
and a driver must declare which it is.

**Billing units.** Wan bills *per successfully generated second of
video*. OpenAI `gpt-image-1` encodes the output image as tokens and
bills per million. Replicate bills per second of GPU time for
non-official models and per output unit for official ones. Stability
and Ideogram bill in credits. Several providers bill per megapixel, so
a 4K image costs a multiple of a 1024² one on the same model. Only the
first of these is expressible in `Usage` today.

**Plan billing.** Alibaba's Token Plan bills in Credits against a
monthly per-seat quota that does not carry over, and **blocks API
calls when the quota is exhausted rather than charging overage**. That
is neither a transient failure nor a request error, which is why the
failover taxonomy needs a third class.

**Async is not one pattern.** Three vendors, three unrelated
protocols. Alibaba returns an opaque `task_id` polled by `GET
/api/v1/tasks/{id}`. Vertex Veo returns an *operation resource name*
(`projects/…/operations/…`) polled by `fetchPredictOperation` — a POST,
not a GET on the handle — signalling completion with `done: true`.
Bedrock returns an `invocationArn` polled by `GetAsyncInvoke`. A driver
interface that assumes a task id in a URL fits exactly one of them, so
the job handle must be opaque and polling must be a driver method.

**Artifacts arrive three ways**: an expiring vendor URL (Wan, 24h),
inline base64 (Veo without `storageUri`), or written into an
operator-owned bucket (Bedrock's `outputDataConfig.s3OutputDataConfig`,
mandatory; Veo's `storageUri`, optional). The third maps onto lobslaw's
existing storage mounts; the first has a deadline, which is why the
poll handler must be reliable rather than best-effort.

**Credentials are not always a static key.** Vertex AI *rejects* API
keys — "API keys are not supported by this API" — and requires a
short-lived OAuth2 token (~1h) minted from a service account or ADC.
Bedrock uses SigV4 request signing rather than a header value. Neither
is expressible as `api_key = "env:…"`, so a provider declares a
credential kind. `CredentialService` already mints and refreshes
short-lived OAuth tokens for skills and should serve providers too.

Sources: [Wan text-to-video API reference](https://www.alibabacloud.com/help/en/model-studio/text-to-video-api-reference) ·
[Qwen Cloud text-to-video](https://docs.qwencloud.com/developer-guides/video-generation/text-to-video) ·
[Model Studio Token Plan overview](https://help.aliyun.com/en/model-studio/token-plan-overview) ·
[Savings Plans and Resource Plans](https://www.alibabacloud.com/help/en/model-studio/savings-plan-and-resource-package) ·
[OpenAI API pricing](https://developers.openai.com/api/docs/pricing) ·
[Replicate billing](https://dodopayments.com/blogs/replicate-billing-model) ·
[Normalised image-model pricing survey](https://invideo.io/blog/ai-image-model-pricing/) ·
[Veo on Vertex AI](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/video/generate-videos-from-text) ·
[Veo model reference](https://docs.cloud.google.com/vertex-ai/generative-ai/docs/model-reference/veo-video-generation) ·
[Bedrock StartAsyncInvoke](https://docs.aws.amazon.com/de_de/bedrock/latest/APIReference/API_runtime_StartAsyncInvoke.html) ·
[Nova Reel text-to-video](https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-runtime_example_bedrock-runtime_Scenario_AmazonNova_TextToVideo_section.html) ·
[Vertex AI authentication](https://docs.cloud.google.com/vertex-ai/docs/authentication)

## R25 — retire the node functions that do not select anything

`[cluster].functions` advertises five roles. Three of them do not
independently select behaviour, and one of those actively misleads.

What each gate resolves to today (`internal/node/wire.go`):

| function | gate | wires |
|---|---|---|
| `compute` | `gateCompute` | agent, tool registry, builtins |
| `gateway` | `gateGateway` (function **and** `Gateway.Enabled`) | Telegram, HTTP |
| `storage` | `gateRaftAnd(gateStorage)` | mounts, storage service |
| `memory` | `gateRaft` | raft, store, memory service |
| `policy` | `gateRaft` | *the same stages as `memory`* |

Findings:

1. **`policy` selects nothing of its own.** `needsRaft = memory ||
   policy`, and both `policy-svc` and `memory-svc` sit behind it, so
   the two bits are indistinguishable: `functions = ["policy"]` yields
   a memory node. Enforcement is not gated on it either — the engine is
   built in `wireCompute` on `n.store != nil`. An operator setting it
   gets something other than what it says, with no warning.

2. **`gateway` is implied by `compute`.** `wireGateway` returns
   "gateway requires compute function" without an agent, so it is never
   independently deployable, and `Gateway.Enabled` is already the real
   switch. Two knobs for one decision.

3. **`memory` and `storage` are mutually required.** Config validation
   rejects memory without storage; the storage stage is gated behind
   raft, which only memory (or policy) provides. Neither can be
   deployed without the other, so they are one role spelled two ways.

4. **Splitting compute from memory does not work today.**
   `MemoryServiceClient` is generated but has no call site, so a
   compute node cannot reach another node's store. A "compute-only"
   node is not a node using remote memory; it is a node with no memory
   at all. Extra memory nodes are useful as raft replicas for
   durability, not as a service other nodes consume.

So the five bits express two real roles: raft-backed state
(memory+storage+policy) and agent (compute+gateway).

**Decision: retire `policy` now** — accept it in config as a
deprecated alias for `memory` so existing configs keep working, warn at
boot, and drop it after a release.

**Decided 2026-08-16: memory is always local raft replicas.** There is
no design plan for a compute node reading another node's store, and
`MemoryServiceClient` stays uncalled. That removes the reason to keep
`memory` separately addressable, so the deferred half proceeds:
`memory` and `storage` normalise to each other, and `gateway` retires
in favour of the `Gateway.Enabled` switch that already existed. Revisit
only if remote memory is ever actually designed.

This also settles the shape of R7 gap 7 (the cluster gRPC principal):
a non-state node is not a thing, so the question is narrower than it
looked.

Acceptance:
- `functions = ["policy"]` boots, logs a deprecation warning naming
  `memory`, and behaves identically.
- A test asserts the alias, so removing it later is a deliberate act.
- `docs/` and `examples/config.toml` stop listing `policy` as a role.

## R26 — a second vendor per generation modality

R22 shipped speak, image and video with exactly one vendor each:
OpenAI-shaped speech, OpenAI-shaped images, DashScope video. One
implementation always fits its own interface, which is the reason the
chat waist got a second driver (Anthropic) before it was trusted.

The generation modalities have not had that test. Until a second
vendor lands behind each, the failover chains built in R22 step 6
route between providers that all speak the same protocol, and the
artifact resolver has only ever seen the delivery modes those three
vendors happen to use.

Worth more than an extension point: a second vendor exercises the
waist for real, and is immediately useful to an operator who has an
account with the other one.

Candidates, chosen to be un-alike rather than convenient:
- speak: a self-hosted Kokoro or Piper behind the same shape (proves
  the driver is not quietly OpenAI-specific), or ElevenLabs (proves it
  is, if it breaks).
- image: an operator-bucket writer, so `ArtifactMount` sees real use.
  Today only the mock produces one.
- video: Veo or Bedrock, whose handles are an operation resource name
  and an ARN. Both were surveyed for R22 and neither has been
  implemented, so `JobHandle`'s opacity is still only asserted.

Acceptance: each new driver passes the existing conformance suite
unchanged. A suite that needs editing to admit the second driver is
the finding, not the driver.

**Status: done.** ElevenLabs (speak), Imagen (image) and Veo (video)
all landed and all passed the suite unchanged. Veo produced the first
real `ArtifactMount` — until then only the mock had — and its
operation-resource-name handle is what finally tested `JobHandle`'s
opacity, which had been asserted and only mock-exercised. All three
are fake-verified; no accounts exist for any of them.

## R27 — the config sweep (done 2026-08-17)

### Problem

Three settings turned up in one week that were parsed, validated,
documented — and read by nothing: `min_trust_tier`, `[[compute.chains]]`
and `skills.signing_policy`. Three in one week is a pattern, not a
coincidence, so the whole surface was swept.

**35 of 296 settings were read by nothing outside `pkg/config`.**

| Section | Dead keys | What the operator believed |
|---|---|---|
| `[sandbox]` | `allowed_paths`, `read_only_paths`, `network_allow_cidr`, `dangerous_cmds_deny`/`_allow`, `env_whitelist`, `cpu_quota`, `memory_limit_mb`, `skip_perm_checks`, `hot_reload_opt_out` | filesystem, egress and resource limits |
| `[[storage.mounts]]` | `endpoint`, `account`, `access_key_ref`, `secret_key_ref`, `env`, `crypt`, `crypt_password_ref`, `crypt_salt_ref`, `extra_opts` | credentials and encryption at rest |
| `[[compute.plugins]]` | `name`, `source`, `enabled`, `auto_install_binary`, `grpc_port` | plugin tools loaded |
| `[audit.raft]` / `[audit.local]` | `anchor_target`, `anchor_cadence` (×2) | tamper-evident anchoring |
| `[memory.snapshot]` | `cadence`, `retention` | snapshots scheduled and pruned |
| `[logging]` | `level`, `format` | log verbosity from config |
| singles | `compute.complexity_estimator`, `discovery.broadcast_interface`, `soul.scope` | |

Every one was in `examples/config.toml`, most were in `DESIGN.md`, and
two were in the end-user security documentation. An operator who wrote
`network_allow_cidr` had restricted nothing, and nothing said so.

### The storage mounts were the worst of it

Not because a setting was ignored, but because the section could not
express a working mount **at all**. `seedStorageMounts` copied label,
type, path and bucket into the replicated proto and dropped the rest —
and there were no `remote`, `server` or `export` keys to drop, because
the struct never had them. The rclone backend refuses a mount whose
remote is empty; the NFS backend refuses one with no server or export.
So `type = "rclone"` and `type = "nfs"` declared in TOML could never
start, and the credentials written beside them reached nothing.

Fixed by giving the section the vocabulary the backends actually speak:
`remote`, `server`, `export`, and one `options` map passed through
verbatim, replacing eight bespoke credential keys. Fewer settings, and
they work.

`crypt` was removed rather than wired: `internal/storage/rclone`
documents crypt as a follow-up, so there is no code to encrypt
anything. **A key promising encryption at rest on a system that cannot
encrypt is worse than no key** — `crypt = true` returned plaintext and
no warning.

### Disposition

- **Deleted** (33 keys): the whole `[sandbox]` parallel vocabulary — the
  sandbox is described by policy.d files, and two authorities for one
  sandbox is how they came to disagree; `[[compute.plugins]]`, because
  `lobslaw plugin install` has always been the thing that installs a
  plugin; audit anchoring and snapshot cadence/retention, which
  described schedules for jobs that do not run.
- **Wired** : `[logging] level`/`format` now reach the logger, after
  config loads rather than at construction — building the logger has to
  come first, because loading config is one of the things that must be
  able to report an error. An explicit `--log-level` still wins:
  the file holds a deployment's normal level, the flag is what somebody
  types while debugging this boot.
- **Replaced**: the storage mount credential fields, as above.

Net: 296 settings → 264.

### The guard

`TestEverySettingIsReadBySomething` fails when a koanf-tagged field has
no reader, with an allowlist for deliberate exceptions that is empty
today. Adding to it is a visible decision in review.

**Type-checked, not grepped.** A textual search for `.Level` cannot tell
`LoggingConfig.Level` from the dozen other structs with a field of that
name — and `[logging] level` was one of the 35, so a name-based check
would have missed exactly the case that motivated it.

Two bugs in the analysis had to be fixed before its output could be
trusted, both of which made it report live settings as dead:
`Tests: true` loads duplicate package variants, so the same field is a
different `*types.Var` in each and object identity breaks; and Go 1.23+
materialises `type SpeakConfig = ModalityOverride` as `*types.Alias`,
which a `*types.Named` assertion skips in silence.

It earned its keep immediately: it caught `StorageMountConfig.Server`
during this very change, when the config edit landed and the wiring edit
silently did not.

## R30 — cross-network node enrolment

### Problem

A node in another network cannot use the broadcast hint — UDP broadcast does not cross subnets —
so it needs `seed_nodes` AND a node certificate copied to it by hand. The copying is the friction,
and it is the same friction R29 removes for operators.

### Why it is not just R29 again

The credential is a different order of grant. An operator certificate is ClientAuth-only, carries
`OU=operator`, and is refused on the raft transport. A node certificate carries `ServerAuth` too,
joins raft as a VOTER, and receives a full replica of memory, sessions and credentials.

So the operator-scoped intermediate from R29 must not be able to sign it. If it could, a
compromised node mints a peer, that peer can mint peers, and one compromise grows without bound.

### Proposal

Same CSR-and-approve shape, different authority:

- the node submits a CSR and the cluster QUEUES it — no online signing key can satisfy it
- issuance happens at the offline CA, one command, once per node
- or an enrolment token minted AT THE CA is presented with the CSR

Nodes are rare; operators are not. Requiring a trip to the CA machine per node is proportionate in
a way it would not be per person.

Worth noting separately: raft across networks has non-credential problems — election timeouts
tuned for a LAN, partition behaviour, and a voter whose round-trip is an order of magnitude worse
than its peers. Solving the credential path does not make cross-network clustering advisable on
its own.

### Acceptance

- [ ] A node can submit a CSR to a running cluster and have it queued.
- [ ] Nothing online can issue a node certificate; issuance requires the offline CA.
- [ ] An operator-scoped intermediate cannot sign a node certificate, and there is a test.
- [ ] The join path documents that discovery is a hint and trust is not.

## R29 — enrolling an operator without moving a private key

### Problem

`cluster sign-operator` generates the keypair ON THE SIGNER'S MACHINE and writes both halves to a
directory. Getting them to the laptop is left to the operator: scp, a USB stick, a password
manager. The command prints a `contexts.toml` block that assumes the files already arrived.

So the private key travels. That is the weakness, and it is upstream of any question about
transport — encrypting it in flight does not stop it existing in a second place.

Delivering it over a channel is worse, not better. A private key sent over Telegram lands in a
message store, a phone backup, Telegram's servers, and — in this system specifically — the node's
own transcript store, where `lobslaw session show` will print it back.

### Proposal

**The key never moves.** The laptop generates its own keypair, keeps the private half, and sends
only a CSR. A certificate and a CSR are both public; they can cross any channel.

- **`cluster sign-operator` becomes `cluster export-operator`**, named for what it does. It stays,
  because bootstrap needs it — the first operator has nobody to approve them — and it carries a
  warning that the private key travels.
- **`lobslaw enrol --addr node:9090 --name alice`** generates ed25519 locally, writes
  `operator-key.pem` 0600, and submits a CSR. The node raises a prompt naming the key fingerprint;
  the approver checks it against what the laptop printed before tapping.
- **A separate operator CA** on the node does the signing. The cluster CA stays offline.

  This started as an intermediate under the cluster CA and could not stay one: `GenerateCA` emits
  `MaxPathLen 0`, so the cluster CA signs end-entity certificates and nothing else. Go rejects a
  chain through it with "too many intermediates for path length constraint" — verified rather than
  assumed. Raising the path length means reissuing the root, which means reissuing every node
  certificate.

  So operator certificates chain to their OWN root, which the node holds. The property is the one
  the intermediate would have given, arrived at more directly: the online key mints ClientAuth
  `OU=operator` credentials, and cannot mint a NODE certificate because peers verify each other
  against the cluster CA and this key is not in that chain. A node compromise buys the ability to
  impersonate operators — bad, and bounded — not the ability to manufacture a peer.

  It is also structural rather than attribute-based. `IsOperatorCert` reads an OU string;
  `ChainsToOperatorCA` reads which key actually signed the certificate, and that is not forgeable
  by naming yourself.
- **Approval** comes from the configured owner over a channel, or from any existing operator via
  `lobslaw enrol approve <id>`. The second path matters: it does not depend on a channel being up.

One piece of out-of-band material is unavoidable. The laptop must authenticate the NODE before
trusting anything it sends, so it needs the CA fingerprint. `enrol` prints what it received and
requires `--ca-fingerprint` to match.

### What this is NOT

Node enrolment. The same CSR-and-approve shape would work for a machine that wants to be a peer,
and it would solve a real problem — UDP broadcast does not cross subnets, so a remote node already
needs `seed_nodes` plus a hand-copied cert. But the authority deliberately does not generalise: if
the node-held intermediate could sign node certs, a compromised node mints a peer, that peer is a
voter with a full replica of memory, sessions and credentials, and it can mint more peers.
Unbounded from one compromise.

There is a proportionality point too. A Telegram tap is a reasonable bar for "let alice's laptop
READ the cluster" — the credential is ClientAuth-only and refused on the raft transport. It is not
a reasonable bar for "add a voting replica that receives a copy of everything". Same button, very
different blast radius. Cross-network node enrolment is R30.

### Acceptance

- [x] A prompt can only be answered by the person it was asked of.
      **A prerequisite, and a live gap in shipped code rather than a new-feature concern.**

      `handleCallbackQuery` resolved a prompt straight from `callback_data` and never looked at
      `q.From`. Prompt ids are 128 bits of randomness, so this was capability-by-obscurity — in a
      DM only that user sees the button — but in ANY GROUP CHAT the bot is in, every member could
      see a pending confirmation and tap Approve. That already applied to tool execution; wiring
      certificate issuance to the same mechanism would have made a tap a cluster takeover.

      The audience is captured when the prompt is RAISED, not read off the answer — the same
      reasoning the "always" grant path already documented: a callback is attacker-shaped input
      and the turn that triggered the confirmation is not. The comparison is
      principal-to-principal rather than raw id to raw id, so it survives an `identity rebind`.

      Fails CLOSED, and says which of the two failures happened. A prompt with no recorded
      audience is a broken RAISE path; a prompt belonging to somebody else is not. Reporting both
      as "not for you" would send an operator looking for the wrong problem.

      The refusal reaches the tapper. Silence reads as a broken button and invites retrying, after
      which they keep tapping and the person who can actually answer never learns there is a
      question waiting.
- [x] `cluster export-operator` replaces `sign-operator` and says the key travels.
      **Renamed because "sign" understated it.** The command hands over BOTH halves of a keypair,
      and the private one then has to reach a laptop somehow. The output now says so and points
      at `lobslaw enrol`, which does not.

      `sign-operator` still works and prints a deprecation note rather than failing. Somebody
      with the old name in a runbook should get the credential they asked for and learn the new
      name at the same time.

      Renaming surfaced a pre-existing bug worth recording: `cluster sign-operator alice --out X`
      had NEVER worked, because Go's flag package stops at the first non-flag argument — and the
      command's own error message told you to use exactly that form. `enrol approve <id>
      --fingerprint X` had the same shape. Both now go through `parseFlagsAndPositionals`, which
      re-parses the tail after each positional.
- [x] A separate operator CA signs enrolments; the cluster CA stays offline.
      **The intermediate design was impossible here, and the replacement is better.**

      `GenerateCA` emits `MaxPathLen 0`, so the cluster CA cannot carry an intermediate at all —
      confirmed empirically against Go's verifier, not inferred. Retrofitting would mean reissuing
      the root and therefore every node certificate.

      The operator root is separate, self-signed, path-len 0 (it signs people, never other CAs —
      a root that could delegate would be a way to smuggle the authority elsewhere), and created
      on first use so nobody has to know it exists before their first enrolment. A half-present
      one is REFUSED rather than regenerated over: overwriting a surviving cert would invalidate
      every credential issued under it, and doing that silently during a routine boot is not
      recoverable from the logs.

      The pools are kept apart. `ClientCAs` is cluster + operator; `RootCAs` stays cluster-only,
      so an operator credential can never be presented BY a server. That separation is what a
      handshake test proves — two mutations survived the field-level assertions, because the
      server config reading the wrong pool and a `Reload` dropping the anchor both leave the
      fields untouched and only show up in an actual handshake.

      `SignOperatorCSR` takes the name from the APPROVER, never from the request. A CSR is
      attacker-shaped input and its subject is whatever the requester typed; the name in the
      certificate has to be the one the approver saw and agreed to. It also checks the request's
      self-signature, without which somebody could get a certificate issued in a name they chose
      for a key somebody else holds.
- [x] `lobslaw enrol` generates its keypair locally and never transmits the private half.
      **The key is born on the laptop and stays there.**

      `enrol request` generates ed25519 locally, writes `operator-key.pem` 0600, and sends only a
      CSR. It refuses to overwrite an existing key: that file may be the private half of a
      credential still valid somewhere, and replacing it is not recoverable. It also refuses
      BEFORE generating anything when `--ca-cert` is missing — reading the CA later would fail
      too, but only after a key had been written, which then blocks the retry via the
      overwrite refusal.

      `--ca-cert` is required and is the one piece of material enrolment cannot avoid needing: a
      laptop that trusted whatever answered would enrol against an impostor and never know.

      Both spellings dispatch. British single-l is canonical — enrol, enrolling, enrolment — and
      the usage teaches only that one; `enroll` is aliased because it is the spelling half the
      world's muscle memory produces, and a typo that prints "unknown subcommand" teaches
      nothing.
- [x] An enrolment is approved by the owner over a channel, or by an existing operator.
      **Both, and the channel path inherits the audience check rather than working around it.**

      `enrol approve` requires `--fingerprint`. An approval without one is somebody clicking yes
      to a request they have not checked is the one they were told about, which is the failure
      this whole flow exists to make hard. The pin is enforced at the SERVER: a request that
      changed between reading and approving is refused rather than approved in place of the one
      that was verified. Denial does not require it — refusing a request you cannot identify is
      the safe direction, and demanding a fingerprint to say no would leave junk requests
      un-closable.

      Every decision names somebody, read from the VERIFIED client certificate rather than from
      anything the request carries. An unattributed approval is an operator credential nobody is
      accountable for.

      The channel path reuses the confirmation machinery instead of adding a second callback
      shape. A pending enrolment raises a prompt carrying the request id, delivered to whoever
      holds the owner scope — so the audience check from the callback-authentication fix applies
      unchanged, and a bystander in a group chat cannot admit an operator. The keyboard is
      approve/deny only: an always-grant for issuing operator credentials would be a standing
      authority to admit anyone who asks.

      Asked AFTER the request is durably queued, and never fatally. A channel outage must not
      lose an enrolment somebody could still approve from the CLI — the request is the record,
      the prompt is only a way of noticing it.

      A failed issue is reported to the person who approved, not merely logged: they tapped
      Approve and would otherwise tell somebody to collect a certificate that does not exist.

      **A real bug surfaced here.** `tgUserIdentity` is not stable across updates — it prefers the
      username and falls back to the numeric id, so the same person is "tg-@alice" from a message
      that carried one and "tg-1" from one that did not. An enrolment prompt is raised from a
      context where nobody has messaged us, so it only has the id, and the audience check refused
      the very person it was raised for. It now matches on either form; the numeric one is the
      stable identity underneath and admits nobody extra. The same latent bug would have locked a
      user out of their own confirmations after they removed their Telegram username.
- [x] The approver sees the key fingerprint, and the laptop prints the same one.
      **And a mismatch is a refusal, not a footnote.**

      The laptop prints the fingerprint of the key it generated; the node echoes what it
      computed from the request. Different values mean the node is describing a key this laptop
      did not generate — so the output says DO NOT APPROVE rather than noting it in passing.

      In the listing the fingerprint gets its own line, because it is the thing a human has to
      compare character by character rather than glance at.

## R28 — the operator's laptop

### Problem

Somebody running a cluster in the cloud should be able to administer it from a laptop. Today they
can do two things.

The transport is not the gap. `liveNode` dials over mTLS with `--addr` plus CA / cert / key (or
`LOBSLAW_NODE_ADDR`), mTLS is mandatory with no plaintext fallback, and `lobslaw skills` and
`lobslaw learned` already work against a remote node. The gap is that **this is the exception**:

| Command | Reaches a remote node? |
|---|---|
| `skills`, `learned` | yes — dials the node |
| `memory`, `policy`, `identity`, `session`, `cluster` | **no — opens `state.db` on the local filesystem** |
| `trace` | **no — reads NDJSON under the node's data dir** |

On a laptop the second row does not fail cleanly. It operates on a database that is not the
cluster's, or is not there at all. An operator listing sessions would see an empty list and have no
reason to doubt it.

### Three things in the way

**1. The credential is a NODE identity.** `cluster sign-node` is the only signing command, so a
laptop presents exactly what a cluster peer presents. Three consequences: an operator's laptop holds
material that could act as a node; revoking one operator means rotating a node cert; and every
action in the audit log is attributed to a node rather than to a person. The CLI comment already
says it presents "the same credential a peer node does" — that was the right call for a tool running
beside the node, and it is the wrong one for a tool running on somebody's laptop.

**2. Most commands have no live form.** Cheaper than it looks for some of them: `Memory`, `Policy`
and `Audit` services are already registered on the cluster listener, so those are a matter of
pointing the CLI at what exists. `Session` and `Identity` have no service yet. `trace` is genuinely
harder — R24 stored traces per-node rather than in raft, deliberately, so "show me the trace" has to
name a node or fan out.

**3. No connection profile.** Four flags on every invocation, or a `config.toml` that a laptop does
not have and should not have — it is the node's file, full of paths that mean nothing off the node.

### Proposal

Sequenced so each step is useful alone.

- **Operator certificates.** `lobslaw cluster sign-operator <name>`, issuing a client certificate
  that is not a node certificate. The cluster listener accepts it for the administrative services
  and refuses it for raft transport, so a stolen laptop credential cannot join the cluster or
  replicate. Audit entries carry the operator name.
- **Connection profiles.** `~/.config/lobslaw/contexts.toml` with named clusters — address, CA,
  operator cert — and `lobslaw --context prod`. `LOBSLAW_CONTEXT` for shells that live in one. The
  node's `config.toml` stays what it is: the node's.
- **Live forms for what already has a service** — `memory`, `policy`, `audit`. This was written as
  "rewiring, not new protocol", and that is true of **audit only**. `AuditService` already has
  `Query` and `VerifyChain` with a sink selector, so the CLI is pure client work. `PolicyService`
  had no way to DELETE a rule, which is what `revoke-approvals` does — though `SyncRules` already
  covered the listing, so only revocation needed protocol. `MemoryService` has `Forget` but nothing
  behind `memory list`, `show`, `share` or `unshare`. The estimate was too optimistic and the
  sequencing should reflect it.
- **Services for what does not** — `session`, `identity`.
- **`trace` names a node.** Per-node storage was a deliberate answer to R24's objection and should
  not be undone to make a CLI easier; the CLI should say which node it is reading, and default to
  the one it is connected to.

**A rule to hold to throughout:** a command that cannot reach the cluster must SAY SO. The failure
mode this is meant to remove is not "the command errored" — it is a laptop-local `state.db` being
read as though it were the cluster's, and answering confidently with nothing in it.

### Acceptance

- [x] `lobslaw cluster sign-operator` issues a credential that administers but cannot join.
      **Two halves, and only both make the claim true.**

      A node certificate carries `ServerAuth` as well as `ClientAuth`, because a node both dials
      its peers and serves them — which is exactly what made handing one to a laptop equivalent
      to handing over a cluster membership. An operator dials and is never dialled, so its
      certificate is CLIENT AUTHENTICATION ONLY and nothing can serve with it.

      That alone is not enough: a peer dials as a client too, so ClientAuth would still permit
      opening a raft stream. The certificate carries `OU=operator`, and the server REFUSES that OU
      on the raft transport — enforced at the server, because a check on the client is one the
      attacker controls, and on the STREAMING interceptor too, because raft's transport is
      streaming and a unary-only guard would cover nothing that matters.

      An unidentified caller is refused on the peer-only path rather than admitted. mTLS is
      mandatory on that listener, so a call arriving with no verified chain is not a configuration
      this cluster has — and guessing in favour of the caller is the wrong way to be wrong about
      consensus.

      The OU rather than a CommonName prefix, because the CN is already the identity every service
      reads for attribution; overloading it would make every reader parse a prefix, and the first
      one that forgot would treat an operator as a node.

      Shorter-lived than a node's by default: a person's credential lives on a laptop that
      travels. Revoking it no longer requires rotating any node's identity, and an audit entry
      names who rather than which host.
- [x] A named context replaces the four connection flags.
      **The precedence is the design, and it puts the context above `config.toml`.**

      `contexts.toml` in the operator's own config directory names clusters — address, CA,
      operator cert and key — reached with `--context prod` or `LOBSLAW_CONTEXT`, with an
      optional `default` for the common case of one cluster. Explicit flags still win, and
      overriding one of them keeps the rest of the context rather than demanding all four again.

      A `config.toml` found on a laptop is likelier to be a leftover from running a node locally
      than the cluster the operator meant to reach, so a named context outranks it. And an
      unknown context name is a hard error listing what exists, not a fallback to the default:
      the failure worth preventing is a command aimed at staging that lands on production.

      `--context` is a global value flag, so `lobslaw --context prod memory list` finds its
      subcommand rather than reading `prod` as one. `cluster sign-operator` prints the
      `contexts.toml` block for the credential it just signed, and a test loads that printed
      block back — a snippet that does not parse sends the operator to a "no such file" error
      about a credential sitting right there.
- [x] `memory`, `policy` and `audit` work against a remote node.
      **All three, though "rewiring, not new protocol" only held for `audit`.**

      `audit query` and `audit verify` now talk to a running node by default, with `--offline`
      as the forensic opt-out for a cluster that will not start. Where the answer came from is
      printed either way, because an empty audit log is indistinguishable from the wrong file
      unless the source is on the page.

      Two refusals carry the R28 rule. `verify` exits non-zero when no sink could be checked at
      all — exiting 0 having verified nothing is precisely the confident-answer-about-nothing
      failure this item exists to remove. And a sink the node does not run is reported as
      unavailable rather than as a broken chain, because conflating the two sends somebody
      looking for tampering that did not happen.

      Sinks are walked one call each rather than in a single combined check: the service
      flattens a combined verification into one verdict, and "the chain is broken" without
      naming the sink is half an answer.

      **`policy` is done, and half of it needed no protocol after all.** `SyncRules` already
      returns every rule with its provenance, so `policy approvals` is a filter on the same
      constant the offline form uses — one authority for "minted by an approval", in
      `internal/policy`. Only revocation needed an RPC.

      `RevokeApprovalRules` is scoped **in its name** rather than by a prefix the caller
      supplies, and the refusal lives on the SERVER. The guarantee an operator relies on —
      that revoking their approvals cannot touch a rule they wrote by hand — is worth nothing
      if the check sits in the client, because a client is what an attacker replaces. There is
      deliberately no unscoped delete: nothing needs one, and an RPC that can remove any policy
      rule is the one you would most like not to have shipped.

      Naming nothing is not everything: an empty id list with `all=false` is an error at both
      ends, so a mistyped command cannot become a blanket revocation. A rule that exists but was
      not minted by an approval is *refused*; an id that does not exist is *not found*. Kept
      apart because they are different mistakes, and "not revoked" without saying which leaves
      the operator to guess.

      **`memory show`, `list` and `forget` are live**, and the scan moved to `internal/memory` so
      both sides of the wire answer with one definition of what each filter means. `share`,
      `unshare` and `consolidations` have no live form yet — they still run, and they SAY SO on
      stderr rather than quietly reading a local `state.db` that is not the cluster's. Announcing
      it beats refusing a command that works to make a point about a flag.

      Two things the live `forget` needed that the RPC did not have. **A dry run** — `memory
      forget` has always been one unless `--apply`, and forget is irreversible, so the remote
      form must not be the one that deletes on the first try. And **the resolved plan**: matched,
      swept, and requested ids that do not exist.

      `Service.Forget` also carried its own copy of the matching, while the comment on
      `ForgetQuery` claimed the CLI and the RPC "cannot diverge on what forget these means" — a
      claim with two implementations either side of it. They share `PlanForgetFor` now, and the
      requester scoping happens where it has to: BETWEEN matching and cascading. A record the
      caller may not read must leave the matched set before the cascade runs, or it pulls its
      consolidations down with it and deletes through a record they were never allowed to see.
- [x] `session` and `identity` have services and live forms.
      **Both, and `identity` was the one that was not a wrapper.**

      `SessionService` is read-only on purpose: forgetting a conversation is a replicated
      mutation with its own path, and browsing what was said does not need one that can also
      delete it. `SearchSessions` goes through the same `SearchTranscripts` the agent's
      `session_search` tool uses, so what an operator sees is what the model would have found —
      two search implementations would eventually disagree, and the operator would be the one
      debugging it.

      The filtering and the transcript scan moved from `cmd/lobslaw` into `internal/memory`, so
      the CLI and the service answer with one definition of what `--channel telegram` selects.

      An unwired service REFUSES rather than answering empty. A service that returns "no
      sessions" because it has no store is indistinguishable from a quiet cluster, which is the
      failure this whole item exists to remove.

      `identity rebind` was not a thin wrapper. Its rewriters lived in `cmd/lobslaw` and wrote
      straight to bbolt, so it needed a stopped node — and pointed at a follower's file while
      the cluster ran, it would have written ownership no other replica has. That is worse than
      requiring the outage, which is why the live path replicates.

      The rewriters produce LOG ENTRIES now rather than raw bytes. That is what makes the
      replicated path possible at all — the FSM dispatches on payload type — and it keeps one
      mapping from record type to bucket, the one `bucketAndPayload` already owned.

      Not atomic across records: raft has no multi-entry transaction here, so a rebind
      interrupted midway leaves some moved and some not. Recoverable by re-running, because each
      rewrite is idempotent, and the failure reports HOW MANY LANDED — "rebind failed" without a
      number leaves somebody unable to tell a no-op from a half-done move. The narrow
      `RebindApplier` interface exists so that path is reachable from a test at all: a rebind
      that reports success after a failed apply is the worst outcome available here, and a real
      raft node does not offer that failure on demand.
- [x] `trace` names the node it read, and does not silently read a local directory when pointed at a
      remote cluster.
      **Per-node storage stayed; what changed is being able to ask a specific node.**

      R24 kept traces out of raft so a trace never costs a replicated write, and that objection
      was right. `TraceService` does not undo it — it lets an operator ask ONE node what it
      recorded, instead of the CLI reading whatever directory is on the machine they typed on.

      Every response carries the node id, and `--offline` names the directory. A stale copy on a
      laptop reported as the cluster's is exactly the failure this item exists to remove.

      A node with tracing OFF is reported distinctly from a node that has served no turns. Both
      have nothing to show and only one is fixed by editing config, so the service returns
      `enabled` alongside the empty listing rather than an error — a deliberate setting should
      not look broken.

      The no-content guarantee survived the move. `SpanToProto` is written field by field rather
      than reflected or marshalled wholesale, so a future field on `Span` that carried content
      would have to be added there ON PURPOSE. That is a decision somebody makes rather than one
      that happens to them.
- [x] Every command either reaches the cluster or refuses; none reads a local `state.db` that is not
      the one the operator meant.
      **Enforced by an inventory test, because a rule nothing checks is a rule that decays.**

      `cmd/lobslaw/reach_test.go` walks every dispatcher's declared surface and fails when a
      subcommand has not been placed on one side of the rule. It checks that anything with both
      forms goes live by default and that the two are genuinely different functions; that every
      subcommand appears in its group's usage; that a group with any offline path advertises
      `--offline`; that an offline-only subcommand is MARKED as such in the usage; that nothing
      is declared in two sets; and that no declared route is nil. A dispatcher missing from the
      inventory is invisible to all of it, so adding one is the deliberate act.

      `learned list` turned out to be live-capable all along — `ListArtefacts` existed with no
      caller here, and the usage text calling list "offline-only" was a claim rather than a
      constraint. On a laptop it printed "the agent has taught itself nothing" about a cluster it
      never contacted.

      What stays offline-only, and why: `learned history` and `rollback` read the version bucket,
      which no RPC exposes; `learned discard` is a bulk archive whose dry run is the only preview
      before archiving everything, and composing it from per-artefact calls would lose that;
      `memory share`, `unshare` and `consolidations` likewise have no RPC. All six now ANNOUNCE
      the gap on stderr and carry an `[offline]` marker in the usage. Announcing beats refusing a
      command that works to make a point about a flag — and it beats running silently, which is
      the failure this item names.
