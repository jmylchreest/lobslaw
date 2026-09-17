# Dependency and migration plan: `compute-teams` and `ui-web`

Written in response to the review on #322, which asked for this before
any restructuring. It says what exists, which contracts are missing,
what moves where, and what has to happen to stored records. It is a
plan, not a description — nothing below is implemented yet.

Scope note: this lands as **one PR**, restructured, rather than the five
in §9 of the review. The capability gates are what make the feature
optional; the package boundaries are what make the gates real. Splitting
the delivery is discussed in "Why one PR" at the end.

## 1 · What already holds, and what does not

Two of the dependency rules are substantially satisfied today:

- **Routes are already registration-gated.** `registerRoutes` mounts the
  bot, inbox, activity, session and config routes only when the
  corresponding registry is non-nil, so "not wired" already means "not
  served" rather than "served and erroring". The capability gates extend
  a mechanism that exists rather than introducing one.
- **Records are already separable from execution.** The registries live
  in `internal/memory` behind CAS services; the drain worker in
  `internal/node` is a separate consumer. A node can hold the buckets
  without running the workers, which is what §8 of the review requires.

**The web/orchestration separation does not hold, and not narrowly.**
It is worth being exact, because the size of this is the main thing that
determines the plan:

```go
type Server struct {
    ...
    agent *compute.Agent
}

func NewServer(cfg RESTConfig, agent *compute.Agent) *Server
```

Every handler is a method on `*Server`, including the shared
`authenticate`. So the REST surface does not merely *reach* a local
agent — it **is** a type that holds one. Eleven non-test files in
`internal/gateway` import `internal/compute`, including `rest.go`,
`conversation.go`, `telegram.go`, `slack.go` and `webhook.go`.

The consequence: moving the console files into `internal/gateway/web`
does **not** by itself let a web node build without compute, because the
shared pieces that sub-package depends on are themselves compute-bound.
`gateway` normalising to `compute` is therefore not a configuration
convention that can be relaxed — it is an accurate statement about the
type, and the review's "existing constraint" is load-bearing.

An earlier draft of this plan called the coupling two stray types
(`compute.TurnRequest` in `rest_bot_chat.go`, `memory.InboxFilter` in
`rest_bots.go`). Those leaks are real and worth fixing, but they are not
the problem; fixing only them would produce a `web` package that still
links the agent, and a green build that proved nothing.

## 2 · Missing contracts

- **A REST surface that does not hold an agent.** The keystone. `Server`
  splits into a transport/auth core with no compute dependency, and the
  turn-running paths move behind an interface. `BotTurnRunner` is
  already this shape for per-bot chat — the work is generalising it to
  the main message path and inverting `NewServer` so the agent arrives
  as an implementation rather than a field.
- **A turn contract independent of `internal/compute`.** That interface
  must take a request/response pair owned by a neutral package, so a
  local and a remote implementation are two implementations rather than
  one implementation and a rewrite.
- **A capability discovery response.** Nothing today answers "what may
  *this user* do here", per review §5. Health reports node functions,
  which is the deployment's view, not the caller's.
- **An explicit owner on every team.** See §5.

This is the largest item in the plan and the one most likely to be
mis-estimated. It touches the channel code, which is the part of
`internal/gateway` the original design note warned against disturbing
("if that stops being true, stop and re-plan"). It is worth confirming
this is wanted at this size before step 1 begins.

## 3 · Target packages

Following the addendum's suggested layout:

| Package | Holds | May import |
|---|---|---|
| `internal/bots` | Bot identity, persona, ownership contracts. Currently only `graph.go`. | types, proto |
| `internal/teams` | Coordinator selection, delegation, inbox drain, team work queues. From `internal/node/bot_inbox_drain.go` and the team half of the tools. | `internal/bots`, compute contracts |
| `internal/gateway/web` | Console routes, console sessions, SPA serving. From `rest_bots.go`, `rest_groups.go`, `rest_bot_chat.go`, `rest_bot_insight.go`, `console_session.go`, `ui/`. | gateway's shared auth, service interfaces |
| `internal/gateway` (shared) | `authenticate`, principal resolution, `jsonErr`, audit — reusable across channels, per the addendum. | — |
| `internal/memory` | Buckets, records, CAS services, archive. **Not** behind any build tag. | — |

Dependency rules, stated so CI can assert them:

- `internal/teams` must not import `internal/gateway/...`.
- `internal/compute` must not import `internal/teams`.
- `internal/gateway/web` must not import `internal/compute` or
  `internal/memory`.
- `internal/node` composes; it does not implement.

**The obstacle.** Per §1, the console handlers are methods on a `*Server`
that holds a `*compute.Agent`, and so is `authenticate`. Moving the files
to a sub-package neither compiles nor decouples as-is.

So the order is forced: extract the shared REST authentication, identity
and error surface into something that does **not** hold an agent, and
only then move the console routes onto it. The addendum asks for the
extraction anyway ("Shared REST authentication, identity and
authorisation remain reusable across channels"); §1 is why it has to
come first rather than last.

It must be done without losing the property the `console-multi-user-auth`
decision relies on: there is no auth middleware, each handler gates
itself, and a table-driven test fails when a route is added ungated.
That test moves with the routes and keeps covering both packages.

The channel handlers (Telegram, Slack, webhook) keep their compute
dependency — they are orchestration and belong on the compute side of
the line. The boundary is between *transport plus authorisation* and
*turn execution*, not between web and everything else.

## 4 · Capability gates

Two new `NodeFunction` values alongside `memory`, `compute`, `storage`:
`compute-teams` and `ui-web`. They fit the existing enum, validation and
`NormalizeFunctions` without a new mechanism — which satisfies "one
authoritative activation mechanism". Feature settings stay in their own
config blocks (`[teams]`, `[gateway.ui]`).

Dependencies, validated at startup:

- `compute-teams` requires `compute`. No web dependency.
- `ui-web` requires a reachable authenticated REST backend. It requires
  neither `compute` nor `compute-teams`.

Disabling a capability must stop registration, not merely refuse calls:
no team tools in the registry, no drain worker started, no console
routes mounted, no seeds written. Ordinary `compute` must not seed teams.

### The four states, kept separate

| State | Question | Mechanism |
|---|---|---|
| Compiled | Is the implementation in this binary? | build tag |
| Enabled | Is it configured on this node? | node function |
| Authorised | May this user use it? | policy + ownership |
| Available | Is a suitable backend reachable? | discovery/health |

A binary built without a capability that the configuration requests must
fail validation with a message naming the build profile — not warn, and
not silently serve a subset.

### The normalisation change

`FunctionGateway` currently normalises to `FunctionCompute`, with the
comment "the gateway needs an agent to hand turns to". That is exactly
what blocks a web-serving node using compute elsewhere.

The change: `gateway` keeps normalising to `compute`, so **every existing
configuration behaves as it does today**. `ui-web` is a new function that
does *not* imply `compute`; a node declaring it without `compute` must
have a remote backend configured, and fails validation if it does not.
Nothing existing is reinterpreted — the constraint is lifted by adding a
function, not by weakening the old one. This is called out in the review
as needing explicit documentation, and it gets a decision record.

Note the sequencing this implies. Until the §2 keystone is done, a node
declaring `ui-web` without `compute` cannot be honestly supported — the
binary would still link the agent, and validation would be asserting a
separation the code does not have. `ui-web` without `compute` is
therefore gated on step 2, and if that step is deferred, `ui-web`
temporarily normalises to `compute` exactly as `gateway` does. That is a
supportable intermediate state; claiming remote-backend support without
it is not.

## 5 · Migration

**Team ownership.** `groupMayModify` currently returns true when
`rec.Owner` is empty, so an unowned team is editable by anyone signed
in. Review §2 asks for this removed, and it should be: ambiguous
ownership must not resolve to public access.

Removal is a breaking change for any team already created without an
owner, so it needs a migration rather than a patch:

1. Every team gets an explicit owner. For a single-user deployment —
   which is every deployment today — that is the configured operator.
2. Where no owner can be established, the team is **inaccessible rather
   than public**, and startup logs which teams and why.
3. `owner == ""` then means "nobody", and `groupMayModify` returns false.

The empty-`group_id`-means-default-team rule stays: it is what makes
upgrades migration-free for *bots*, and it is a read mapping, not an
authority decision.

**Records without execution.** Schemas, buckets and `archiveKinds` stay
outside the build tags. A binary built without `compute-teams` must
still back up, export, import and restore team records — otherwise a
reduced binary silently discards data it merely cannot run. Restored
queued work stays paused until explicitly enabled; archives never
restore console sessions or credentials.

## 6 · Build profiles and CI

A small documented set, each built and tested in CI — the review is
explicit that untested build tags are not acceptable:

| Profile | Tags | What it proves |
|---|---|---|
| `core` | `-tags noteams,noweb` | The single-assistant deployment still builds and passes with neither capability compiled in. |
| `teams` | `-tags noweb` | Headless teams over Telegram/Slack/REST/scheduler. |
| `web` | `-tags noteams` | Console against a single assistant, no teams. |
| `full` | *(default)* | Everything. |

Constraints sit at package and wiring boundaries — registration files and
the embed — never scattered through business logic.

## 7 · Sequencing

Ordered so each step is independently reviewable within the one PR, and
so the suite is green at every boundary:

1. **Split `Server`** into a transport/auth core that holds no agent,
   with turn execution behind an interface. Neutral contract types
   replace `compute.TurnRequest` and `memory.InboxFilter` at the
   boundary. No behaviour change, but the largest and riskiest step —
   see §2.
2. Move packages: `internal/teams`, `internal/gateway/web`. Mechanical
   once step 1 lands, and close to impossible before it.
3. Add the two node functions, startup validation, and registration
   gating. Behaviour change: capabilities become real.
4. Capability discovery endpoint and the UI states that consume it.
5. Ownership migration and removal of the unowned-team fallback.
6. Build tags, profiles and CI matrix.
7. Decision records and docs.

Steps 1–2 are refactors that should be reviewable by diff shape alone,
and step 1 is where the review effort should go.

**If step 1 is too large to take now**, steps 3–7 still deliver the thing
this was asked for: the feature becomes genuinely optional, compiled out
by tag and off by configuration, with ownership fixed. What is deferred
is only the web-node-without-compute deployment, and `ui-web` normalises
to `compute` until then. That is a coherent place to stop and is
probably the right first PR if the answer to §2 is "not at that size
yet".

## 8 · Why one PR

The review suggests five. The argument for one, offered for
disagreement rather than as a settled matter: steps 1–3 are pure
refactors whose only justification is step 4, and a refactor PR that
cannot state the capability it enables is harder to review, not easier.
Splitting also means the authorisation fixes already made here land
across several PRs, which the review itself flags as needing care.

The sequencing above is the compromise: one PR, but ordered so it can be
reviewed as if it were several, with the mechanical steps separated from
the behavioural ones.

## 9 · Explicitly out of scope

Per review §2, sharing between human users — membership, cross-user
memory visibility, credential delegation, revocation and audit — is a
separate feature with its own design. Nothing here grants a user access
to another user's bots, and the ownership model is deliberately
single-owner so that the sharing design is not pre-empted.

`/v1/plan` has no auth gate. It predates this work and is untouched by
it; it wants its own issue.
