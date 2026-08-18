# lobslaw — Gateway (channels)

The gateway is the user-facing edge. It turns inbound REST / Telegram traffic into `compute.ProcessMessageRequest` calls on the agent loop, then turns the agent's response back into a channel-appropriate reply (JSON, or a Telegram message with inline buttons).

Three packages cooperate:

- `internal/gateway` — the REST server (`Server`), the Telegram webhook handler (`TelegramHandler`), and the confirmation registry both channels share (`Prompts`, with an in-memory and a raft-backed implementation).
- `pkg/auth` — JWT validation (`Validator`, `ExtractBearer`) used by the REST server to authenticate inbound requests.
- `internal/compute` — the agent loop, tool registry, executor, budget, and mock/real LLM providers the channels drive.

The agent loop knows nothing about HTTP or Telegram. Each channel is a thin adapter that translates inbound transport into an internal request.

---

## Request flow — REST

```mermaid
sequenceDiagram
  autonumber
  participant Client
  participant Server as gateway.Server
  participant Auth as pkg/auth.Validator
  participant Agent as compute.Agent
  participant Prompts as gateway.PromptRegistry

  Client->>Server: POST /v1/messages<br/>Authorization: Bearer <jwt>
  Server->>Server: MaxBytesReader(1MB) + JSON decode
  Server->>Auth: Validate(token)
  alt token missing/invalid + RequireAuth
    Auth-->>Server: error
    Server-->>Client: 401
  else ok
    Auth-->>Server: *types.Claims
    Server->>Agent: RunToolCallLoop(req with Claims, TurnBudget)
    alt resp.NeedsConfirmation && Prompts configured
      Agent-->>Server: resp
      Server->>Prompts: Create(turn, reason, "rest", TTL)
      Prompts-->>Server: Prompt{ID,...}
      Server-->>Client: 200 {reply, needs_confirmation:true, prompt_id}
    else plain reply
      Agent-->>Server: resp
      Server-->>Client: 200 {reply, tool_calls, budget}
    end
  end
```

Client then polls `GET /v1/prompts/<id>` for state and confirms via `POST /v1/prompts/<id>/resolve` with `{"approve": bool}`.

### Routes

| Method + Path | Purpose | Status codes |
|---|---|---|
| `POST /v1/messages` | Main user entry — sends a message to the agent | 200, 400, 401 (w/ RequireAuth), 500, 503 (no agent) |
| `GET  /healthz` | Liveness — process alive | 200 |
| `GET  /readyz` | Readiness — server bound + agent configured | 200, 503 |
| `GET  /v1/prompts/<id>` | Fetch prompt state | 200, 404 |
| `POST /v1/prompts/<id>/resolve` | Approve/deny a confirmation | 200, 400, 404, 409 (already resolved) |
| `POST /telegram` | Telegram webhook (if `Telegram` configured on the server) | 200, 401 |

### Auth modes

The `RESTConfig` `JWTValidator` + `RequireAuth` pair gives four regimes:

| Validator | RequireAuth | Behaviour |
|---|---|---|
| configured | false | Valid JWT → use its scope. Missing/invalid → fall back to `DefaultScope` with a warn log. Good for localhost + reverse-proxy-terminated deployments. |
| configured | true | Reject missing/invalid with 401. Use the token's scope on success. Good for internet-reachable deployments. |
| nil | false | Everyone gets `DefaultScope`. Good for trusted environments / development. |
| nil | true | **Fail-closed.** Every request 401. Intentional: "I asked for auth but provided no validator" is an operator error that shouldn't silently allow traffic. |

Validated tokens with a missing `scope` claim default to `DefaultScope` rather than an empty string.

---

## Request flow — Telegram

```mermaid
sequenceDiagram
  autonumber
  participant TG as Telegram
  participant Handler as gateway.TelegramHandler
  participant Agent as compute.Agent
  participant Prompts as gateway.PromptRegistry

  TG->>Handler: POST /telegram<br/>X-Telegram-Bot-Api-Secret-Token: <secret>
  Handler->>Handler: constantTimeEq(header, WebhookSecret)
  alt mismatch
    Handler-->>TG: 401
  else ok
    Handler->>Handler: firstSeen(update_id) — dedup
    alt duplicate
      Handler-->>TG: 200 (silent drop)
    else new
      Handler->>Handler: resolveScope(user) via UserIDScopes / UnknownUserScope
      alt unmapped user + no default scope
        Handler-->>TG: 200 (silent drop)
      else
        Handler->>Agent: RunToolCallLoop(req)
        alt NeedsConfirmation && Prompts configured
          Agent-->>Handler: resp
          Handler->>Prompts: Create(turn, reason, "telegram", TTL)
          Prompts-->>Handler: Prompt{ID}
          Handler->>TG: sendMessage + reply_markup.inline_keyboard<br/>[Approve|Deny] with callback_data "prompt:<verb>:<id>"
          Handler-->>TG: 200
        else plain reply
          Agent-->>Handler: resp
          Handler->>TG: sendMessage(text)
          Handler-->>TG: 200
        end
      end
    end
  end
```

### Callback query resolution

When the user taps a button:

```mermaid
sequenceDiagram
  autonumber
  participant TG as Telegram
  participant Handler as gateway.TelegramHandler
  participant Prompts as gateway.PromptRegistry

  TG->>Handler: POST /telegram<br/>{update_id, callback_query{id, data:"prompt:approve:<pid>"}}
  Handler->>Handler: auth + firstSeen
  Handler->>TG: answerCallbackQuery(id)  # stops the client spinner
  Handler->>Handler: parse callback_data "prompt:<verb>:<id>"
  alt data shape unknown
    Note over Handler: debug-log + ignore
  else
    Handler->>Prompts: Resolve(id, Approved|Denied)
    alt ErrPromptNotFound
      Handler->>TG: sendMessage("That prompt no longer exists.")
    else ErrPromptResolved
      Handler->>TG: sendMessage("That prompt was already resolved.")
    else ok
      Handler->>TG: sendMessage("Approved." / "Denied.")
    end
  end
```

### Update dedup

`firstSeen` stores each `update_id` with the time it was processed, reaping entries older than 5 minutes opportunistically on every call. This makes Telegram's retry-on-network-error semantics idempotent — the second delivery of the same update_id is silently dropped so the agent doesn't run twice.

For personal-scale deployments (tens of updates/minute) the O(n) per-call reap is fine. A much busier deployment would want an LRU.

### Webhook secret compare

`constantTimeEq` does a length-prefix-tolerant constant-time comparison. Unequal lengths still do a timed XOR of the first operand against itself, so attackers can't detect "right-length but wrong secret" vs "wrong length" via timing.

---

## Confirmation prompt flow

Channels talk to the `Prompts` interface. Two implementations exist:

| Implementation | Used when | Behaviour |
|---|---|---|
| `PromptRegistry` | the node hosts no raft | in-memory, per-process. A `time.AfterFunc` handles expiry. |
| `RaftPrompts` | anywhere raft is present | the record lives in Raft. Resolution is a CAS from `PENDING`, so first-writer-wins is cluster-wide. |

A conformance suite (`prompts_conformance_test.go`) runs both through the same contract, so swapping them does not change what a user sees.

Each prompt has:

- `ID` — random from `crypto/rand` (16 bytes → 32 hex chars).
- `TurnID` — the original agent turn this blocks on; threaded through for audit correlation.
- `Reason` — rendered to the user verbatim.
- `Channel` / `ChannelID` — where the resolution gets delivered. A node that did not ask the question needs `ChannelID` to reply to it.
- `SessionID` — the conversation the resumed leg is appended to.
- `Action` / `Resource` — the operation being confirmed, so a `session` or `always` answer records a grant that matches. Empty for a budget confirmation: spend is not an operation, and a button that silenced future budget warnings is the last thing an operator wants on that prompt.
- `Decision` — transitions once from `Pending` to `Approved` / `Denied` / `TimedOut`. First writer wins.
- `Scope` — `once` / `session` / `always`. Meaningless until `Decision` leaves Pending, and forced to `once` on a denial: "no, and never again" is not something any button offers.
- `Continuation` — the paused turn (see below).
- `ExpiresAt` — after this the prompt times out. In-memory uses a timer; raft-backed closes it when a waiter notices, with a leader-pinned sweeper as the backstop for prompts nobody is waiting on.

### Registry semantics

| Method | Contract |
|---|---|
| `Create(NewPrompt)` | Register a new Pending prompt, carrying whatever the answering node needs. Returns the `Prompt`. |
| `Get(id)` | Snapshot of current state, or `ErrPromptNotFound`. Snapshot so external mutation can't corrupt the stored entry. |
| `Resolve(id, Approved\|Denied, scope)` | Atomic check-and-set — under `r.mu` in memory, a revision CAS in raft. Returns `ErrPromptNotFound`, `ErrPromptResolved` (already decided), or nil. Rejects `Pending` / `TimedOut` — those are internal transitions only. |
| `Wait(ctx, id)` | Blocks until the `resolved` channel closes (Resolve or timeout) or ctx cancels. Returns the final Decision or ctx.Err(). |
| `Reap()` | Sweeps non-Pending entries whose ExpiresAt is in the past. Pending entries are left to their timer; Reap never forces a transition. |

The atomicity of Resolve matters: a split lock would let multiple concurrent callers each pass the Pending check and each return nil, even though only one actually mutated state. The `TestPromptRegistryConcurrentResolveOnlyOneWinner` test pins this behaviour — exactly one goroutine sees `nil`, everyone else sees `ErrPromptResolved`.

### Approval scope

| Scope | Effect |
|---|---|
| `once` | This turn only. |
| `session` | A `SessionApprovals` grant, keyed by conversation, living only in the process — a restart ends the continuity the user was reasoning about. |
| `always` | Mints a policy allow rule with `created_by = "approval:<prompt_id>"`. See [Policy Engine](/security/policy-engine) for the constraints on minting and `lobslaw policy approvals` / `revoke-approvals` for listing and undoing them. |

Telegram renders one button per available scope; REST takes `{"approve": true, "scope": "session"}`. An unrecognised scope narrows to `once` — a typo must never widen a grant.

No scope can reach past the [hardline floor](/security/hardline-floor).

---

## JWT validator (pkg/auth)

Two validation modes, independently configurable — a deployment can enable either, both, or neither.

### HS256 (shared secret)

- `NewValidator(Config)` with `AllowHS256=true` + `HS256Secret=<≥32 bytes>`.
- Good for single-node / personal deployments where a shared secret between the token minter and the server is the simplest answer.
- Secret is pre-resolved; the caller translates `jwt_secret_ref` (e.g. `env:JWT_SECRET`) via the same resolver used for LLM API keys.

### JWKS (RS256/384/512, ES256/384/512, EdDSA)

- `NewValidator(Config)` with `JWKSURL="https://idp.example/.well-known/jwks.json"`.
- Keys fetched lazily on first validation, cached in memory, refreshed every `JWKSRefreshInterval` (default 10m).
- **Key rotation:** a token whose `kid` isn't in the cache force-refreshes once (rate-limited to `JWKSForceRefreshMin`, default 30s) — so an IdP rotation is picked up automatically without operator action, but a flood of bogus `kid` values can't DDoS the IdP through us.
- **Stale-beats-dead:** a refresh that HTTP-errors or returns malformed JSON leaves the existing cache intact and logs a warn. Auth stays up through transient IdP hiccups.
- Supported `kty`: `RSA`, `EC` (P-256/P-384/P-521), `OKP` (Ed25519).
- **Alg/key type coherence:** the validator refuses any combination the IdP didn't advertise — an ES256-header token presented against an RSA JWK is rejected before verification. Blocks classic alg-confusion attacks.

### Shared guarantees (both modes)

- Strict alg allowlist. Accepted algs are the union of what each enabled mode permits; `alg=none` is never accepted.
- Optional `Issuer` claim check.
- `Validate(token)` → `*types.Claims{UserID, Scope, Roles}`. The raw token is parsed, verified, and discarded — never returned.
- `ValidateContext(ctx, token)` plumbs a ctx through to the JWKS fetcher so a hung IdP doesn't stall a request indefinitely.
- `ExtractBearer(header)` — case-insensitive `Bearer` prefix + whitespace-tolerant.

### Construction-time validation

Misconfiguration fails at `NewValidator` rather than on the first inbound request:

- `AllowHS256=true` with no secret → error.
- Secret supplied but `AllowHS256=false` → error.
- No HS256 AND no JWKS → `Validate` always returns `ErrNoValidator`. The REST server maps this to anon-fallback (when `RequireAuth=false`) or 401 (when `RequireAuth=true`).

---

## Mounting

The REST server's `Start` wires:

```
mux.HandleFunc("/v1/messages", ...)
mux.HandleFunc("/healthz", ...)
mux.HandleFunc("/readyz", ...)
if Telegram != nil  { mux.Handle("/telegram", Telegram) }
if Prompts != nil   { mux.HandleFunc("/v1/prompts/", handlePrompt) }
```

Telegram shares the REST server's listener — one TLS cert, one port, one log stream. A deployment that wants to run them on separate ports can spin up two `Server`s with different RESTConfig values and wire the same agent into both.

### Address binding

`RESTConfig.Addr` defaults to `":8443"`. Tests pass `"127.0.0.1:0"` to let the OS pick an ephemeral port, then read `srv.Addr()` after `Start` binds. `Addr()` is empty before `Start` — both tests and production code rely on this.

---

## Binary wiring (Phase 6h)

`node.Node.wireGateway` constructs the `gateway.Server` when both `FunctionGateway` is enabled for the node AND `cfg.Gateway.Enabled = true`. Either gate keeps the HTTP surface dormant — a gateway node that's in the cluster but waiting to be cut-in can boot without binding a user-facing port.

```
cfg.Gateway.Channels[]   -> switch ch.Type { case "telegram": buildTelegramHandler(ch) ... }
```

The channel list is the extension point. Adding a new backend (Slack, Matrix, Signal) is a new `case` in `wireGateway` plus a handler package; the config shape stays stable. Unknown types log a warn and skip so a typo in one entry doesn't prevent the rest of the server from coming up.

### Secret resolution

Channel-level secrets (`bot_token_ref`, `secret_token_ref`) go through `node.Config.ChannelSecretResolver` if set, else the node's default `APIKeyResolver` (the same `env:`/`file:` scheme LLM providers use). Tests inject canned secrets via `ChannelSecretResolver`; production reads them from env/file.

Empty resolved secrets fail boot loudly — a Telegram channel with no bot token is a silent drop of every webhook, so we surface the misconfig at `node.New` time rather than silently accepting updates that never reach the agent.

### Port + TLS

`cfg.Gateway.HTTPPort` defaults to `8443`. Pass `0` for "OS-picked ephemeral port" (tests rely on this). If any channel in `Channels` supplies `tls_cert` + `tls_key`, that pair fronts the whole REST surface — Telegram's webhook demands TLS, so a deployment that lists Telegram automatically gets HTTPS on `/v1/messages` too. No channel with TLS → plaintext (localhost / reverse-proxy-terminated deployments).

### Gateway without compute

A node with `FunctionGateway` but no `FunctionCompute` has no agent to dispatch to. `wireGateway` returns an error from `node.New` rather than silently serving 503s — operators see the misconfiguration at boot, not at first message.

## Agent auto-resume

When an agent turn returns `NeedsConfirmation`, both channels now resume automatically on approval:

- **REST:** `handleMessages` long-polls `PromptRegistry.Wait` in a loop. Approved → `agent.ResumeFromConfirmation` with a `Budget.Relax()`'d turn budget, then loop again if the resumed turn hits another confirmation. Denied / TimedOut → short "Confirmation denied/timed_out: <reason>" reply. Client sees exactly one response per `POST /v1/messages`, whether or not a confirmation was involved.
- **Telegram:** `handleMessage` sends the inline keyboard and stores the paused turn as the prompt's `Continuation`. `handleCallbackQuery` on Approve reads it back, relaxes the budget, calls `agent.ResumeFromConfirmation`, and `sendMessage`s the final reply. Because the continuation is on the record rather than in a handler's map, the tap can land on a node that never sent the keyboard, and an approval after a restart resumes the turn instead of telling the user to resend.

Caps are lifted for the remainder of the turn via `TurnBudget.Relax()` — semantically, the user's Approve means "I authorize this turn to continue however it needs to." Counters are preserved so audit records the full trail.

### What the continuation carries

Conversation state: the transcript so far, the user's message, claims, system prompt, model, timezone, summary, recall, and the budget already spent.

Two deliberate omissions:

- **Tools.** Node state, rebuilt from the resuming node's own registry. A serialised tool definition would outlive the redeploy that changed it.
- **Budget caps.** Read from the resuming node's current config, so an operator lowering a limit is not overridden by a turn that started before the change. Only the *counters* are restored, and only ever forward — otherwise a confirmation would be a way to reset the allowance.

## Who a message is from

`tgUserIdentity` derives `tg-@<username>` when the sender has one, falling back to the numeric id.
That is fine for a log line and wrong as an identity: Telegram usernames are changeable, and a
freed handle can be claimed by somebody else. Bound to the handle, a rename orphans a person's
history and grants, and the next holder of the handle inherits them.

So the handler resolves at the edge — the only place that still has the numeric id — through
`identity.Resolver.ResolveChannel("telegram", <numeric id>, <derived id>)`. An operator binds the
stable address once:

```toml
[[user]]
id = "alice"
channels = [{ type = "telegram", address = "123456789" }]
```

and `claims.UserID` becomes `alice` regardless of what handle they are using.

Unbound senders fall back to the derived id, so a deployment that declares no bindings behaves
exactly as before and a new person can talk to the bot without a config edit first. A lookup
failure logs and falls back too — an outage must not reassign somebody's identity or lock them out
of their own history.

### Binding somebody who already has history

Binding re-points that person at a new canonical id. Everything they own was written under the old
one, so it stays there — not deleted, not theirs any more, and invisible to them.

Two ways to handle it:

**Carry it over.** Stop the node and rebind:

```
lobslaw identity rebind tg-@alice alice          # dry run, shows what would move
lobslaw identity rebind tg-@alice alice --apply
```

It rewrites owners on vector and episodic records, commitments, scheduled tasks, prompts, session
`user_id`s, and policy rules whose subject is that person — including the ones an "always" approval
minted. It does **not** touch `role:` or `scope:` subjects, which name a group rather than a
person, and it will not merge two `user_prefs` records: prefs are keyed by the id itself, so it
reports the conflict and leaves both in place for you to reconcile by hand.

**Or start clean.** Bind the person and let them re-approve what comes up. Nothing is lost that
matters if the deployment is young — memory they cared about can be re-stated, and grants re-given
the next time each one is asked for. This is the simpler path and it is a legitimate choice.

## Responsiveness

A turn takes seconds. What the user sees during those seconds used to
be a Telegram-only concern, written against `*TelegramHandler`; REST
got none of it and every future channel would have reimplemented it or,
more likely, not.

The timers now run against a `Responder` (`internal/gateway/responder.go`):

```go
type Responder interface {
    Typing(ctx context.Context) error
    Interim(ctx context.Context, text string) error
    Final(ctx context.Context, text string) error
}
```

| Timer | Default | What it does |
|---|---|---|
| `TypingInterval` | 4s | Re-signals presence. Under Telegram's ~5s clear window so the indicator reads as continuous. Fires immediately, not after one interval — that first interval is exactly when a user decides nothing is happening. |
| `InterimTimeout` | 30s | One progress message. Single-shot; repeating it doesn't make the turn faster and reads as a stuck loop. |
| `HardTimeout` | 90s | Caps the turn. Applies to every channel, including ones with nobody watching. |

Zero on any field takes the default; **negative disables** that timer,
which is how a channel with no live viewer opts out of typing without
opting out of the hard timeout.

Interim messages are gated on the SOUL's `EmotiveStyle.Directness`
(≥7 skips them): a terse personality emitting "still working on this…"
reads as a different assistant than the one the operator configured.
No SOUL wired means they are emitted — the absence of a personality is
not a terse one.

The timers are tested once, against a fake `Responder`. Adding a
channel should not mean adding timer tests.

### Per channel

| Channel | Typing | Interim | Final |
|---|---|---|---|
| Telegram | `sendChatAction` | `sendMessage` | `sendMessage` |
| REST (SSE) | `event: typing` | `event: interim` | `event: final` |
| REST (JSON) | no-op | no-op | handler writes the body |

A REST client opts into streaming with `Accept: text/event-stream`.
Both the header **and** a flushable `ResponseWriter` are required —
buffering an SSE stream delivers every "progress" event at once, after
the turn it was narrating has finished. A client that does not ask is
byte-for-byte unchanged.

The **hard timeout applies either way**, and that is the part that
matters most: REST had no cap at all, so a stalled provider hung the
request until the client gave up.

### Why the hard timeout used to not work

Worth recording, because the failure was in the code meant to handle it
gracefully. On timeout the agent produces a "this took too long" reply
rather than a bare error, and that call needs a *fresh* context — the
expired one would cancel it before it started.

It used `context.Background()`. So a provider that had stopped
responding — the usual reason a turn hits its cap — was re-entered with
a context that could never cancel, and the request hung anyway.

The summary call is now bounded by `AgentConfig.SummaryTimeout`
(default 15s), built with `context.WithoutCancel` so it survives the
expiry without inheriting immortality. Worth lowering alongside a short
`HardTimeout`: a 15s tail on a 30s cap is most of the budget again.

### What is deliberately not shared

Confirmation rendering. R10 proposed a `Prompt` method on `Responder`,
and the two channels differ genuinely: Telegram sends an inline
keyboard and waits for a callback, REST returns a prompt id and
long-polls. Both work. An interface over two things that differ that
much, for a third channel that does not exist, would be a guess rather
than a generalisation — it can be added when a channel arrives that
makes the right shape obvious.

## Conversation history

Both channels get prior context from `conversationLog` (`internal/gateway/conversation.go`), a two-tier store:

```
channel turn
     │
     ├── Load  ──► durable SessionStore (raft) ──► falls back to cache on error or empty
     │                                                        │
     └── Append ──► in-memory cache (always) ─────────────────┘
                └── durable SessionStore (best-effort; leader-only)
```

The durable tier is `memory.SessionService`, adapted at the wiring layer by `internal/node/wire_sessions.go` — the gateway holds only its own `SessionStore` interface, the same decoupling used for `ChannelStateStore`.

Append never fails a turn. Losing history is bad; failing a turn the agent already completed — after tools ran and the user got their reply — is worse. A write rejected because this node isn't the raft leader is translated to `gateway.ErrSessionUnavailable` and logged at debug, since it's expected rather than broken.

With no session store wired (a gateway-only node, or a test), the whole thing degrades to the pre-session in-memory behaviour.

### Per-channel conversation identity

| Channel | Session id | Default |
|---|---|---|
| Telegram | `telegram:<chat_id>` | always on — one thread per chat |
| REST | `rest:<session_id>` | **off** unless the request carries `session_id` |

REST is opt-in because it has no natural conversation boundary. A script firing independent one-shot requests under a single token must not accumulate them into one ever-growing thread, so omitting `session_id` keeps the original stateless behaviour:

```json
POST /v1/messages
{"message": "what did I just ask you?", "session_id": "cli-42"}
```

REST persists after the confirmation loop resolves, so an approved-and-resumed turn is recorded once and complete rather than twice in halves.

See [MEMORY.md → Sessions](MEMORY.md#sessions) for the storage layout, the trimming rules, and the leader-only caveat.

## What's not yet shipped

Callouts deferred past Phase 6h:

- **`GET /v1/plan` and `GET /v1/health`.** Owned by Phase 7 (scheduler) and Phase 11 (audit) respectively.
- **ACME / Let's Encrypt.** TLS certs are passed explicitly; automatic issuance isn't wired.
- **REST cross-node resume.** REST holds the connection open and resumes in the request that raised the prompt, so it stores no continuation. A REST turn approved elsewhere still records the decision, but the original request has to be re-sent.

See [PLAN.md Phase 6](../../PLAN.md#phase-6-channels-rest--telegram--shipped) for the shipped-scope summary.

---

## Conversation-scoped approvals are replicated

"Approve for the rest of this conversation" used to be honoured out of
a `map[string]struct{}` in one process. The argument for that was real
— a grant outliving what the user was looking at is one they did not
knowingly give — but it only ever covered the **restart** axis.

On the **cluster** axis it never held. Same conversation, same
continuity the user was reasoning about, and they were asked again
because the next message landed on a different node. That is not the
continuity ending; that is routing.

Grants now live in `BucketSessionGrants`, keyed
`<session_id>\x00<action>\x00<resource>` — NUL-separated so a channel
id containing the separator cannot forge a key belonging to another
conversation, and prefixed by the same `<channel>:<channel_id>` key
`SessionRecord` uses so a conversation's grants can be dropped
alongside its transcript.

### The bound is explicit now

The process exiting used to be the TTL. That made the lifetime of a
security grant a function of deploy cadence — weeks on a stable
cluster, ninety seconds during a rollout — and neither of those is a
decision anybody made.

```toml
[security]
session_grant_ttl = "24h"   # default
```

A day, because the unit the user was reasoning about is a conversation
and conversations are a day-shaped thing.

**Expiry is enforced on read**, not by the sweeper. A grant revoked
only when a background pass gets round to it is live for however long
that pass is behind, and "how stale is the sweeper" must not be a
question a permission check has an answer to. The hourly leader-gated
sweep is bucket hygiene — it stops one dead record accumulating per
confirmation ever answered.

**A grant with no expiry is treated as expired, not as eternal.** Every
path that creates one writes the field, so a record without it is one
this code did not write, and the safe reading of "I do not know when
this stops" is that it already has.

### What a node with no raft gets

The in-process map is kept alongside the replicated store rather than
replaced. Two consequences, both deliberate:

- A raft apply can fail — lost leader, timeout — and the user has
  already tapped the button. The local grant means the conversation
  they are in the middle of continues, degraded to what it was before
  rather than broken.
- A gateway on a compute-only node has no local raft, so it keeps the
  process-local map: the behaviour it always had, confined to one
  process, rather than a silently missing feature.

The local map is also consulted first, so an ordinary same-node grant
does not pay a store read on every policy check.

### Forgetting a conversation

`SessionGrantStore.RevokeSession` drops every grant belonging to one
conversation, expired ones included. A cleared conversation must not
keep privileges the user believes they revoked — and "forget" should
be a statement about what is *stored*, not about what is currently
enforceable.

---

## Review notices

`propose` mode fills a queue nobody is told about. The CLI can show it,
but only to somebody who already suspects it exists — and a queue you
have to remember to check is one you check once. Meanwhile the curator
expires unreviewed proposals, which turns *"nobody was told"* into *"a
decision was made by timeout"*.

So the notice **rides out on a turn the user is already having**:

```
…the answer to whatever they asked…

———
2 skills waiting for approval, 1 refinement waiting for review — `lobslaw learned pending --all`
```

That is the whole design. No push mechanism, no per-channel addressing,
no delivery guarantees to get wrong. **Any channel that can send a
reply can carry it**, which is what makes adding a channel later
configuration rather than code — there is a test that a channel the
code has never heard of works the moment it is named.

### Opting in

```toml
[self_learning.notify]
channels = ["telegram"]
subjects = ["user:tg-@john"]
interval = "24h"      # default
```

**Both allowlists must match.** Not a boolean, and not either-or:

- `channels` decides **where**. Adding Slack later is a string.
- `subjects` decides **who**, and that cannot be inferred — the person a
  notice concerns is the one who can act on it, and nothing in the
  conversation says who that is.

A channel allowlist on its own would tell a group chat what the
operator has pending. Empty means none, in both directions: silence is
the safe default for a feature that reports a review queue.

The source is owner-scoped too, so being permitted to receive notices
does not make somebody able to learn what a *different* principal has
pending.

### Three details

**Appended to the outbound text only — never to the transcript.** A
notice recorded as an assistant message is one the model reads next
turn and reasons about, at which point the agent is discussing its own
pending proposals with the user. And it is in the summary forever.

**One per conversation per interval, default 24h.** A nudge appended to
every turn stops being information within an hour, after which it is
read past — worse than never having sent it. The interval is marked on
the **send**, not on the attempt: marking on the attempt would let an
empty queue today silence a full one tomorrow.

**It never fails a reply.** A notice is a courtesy riding on somebody
else's turn; failing a reply the user is waiting for because the
courtesy could not be assembled is the wrong trade in every case.

The rate-limit state is per-process, deliberately — unlike session
grants. The consequence of getting it wrong is one extra line on one
reply, which does not earn a raft round trip on the reply path.

---

## Operator credentials

An operator administering the cluster from a laptop needs a credential
that is **not** a node's.

```console
$ lobslaw cluster export-operator alice \
    --config /etc/lobslaw/config.toml \
    --out ~/.config/lobslaw/prod
Signed an OPERATOR certificate for "alice":
  cert: ~/.config/lobslaw/prod/operator.pem
  key : ~/.config/lobslaw/prod/operator-key.pem
  ca  : ~/.config/lobslaw/prod/ca.pem
```

### Why not a node certificate

A node certificate carries `ServerAuth` as well as `ClientAuth`,
because a node both dials its peers and serves them. Handing one to a
laptop hands over the ability to present as a cluster member — and
revoking that person then means rotating a node's identity, while every
action they took is attributed to a host rather than to them.

### Administers but cannot join

Two halves, and only both make it true:

- **Client authentication only.** Nothing can serve with it, so it
  cannot answer connections as a node.
- **`OU=operator`, refused on the raft transport.** ClientAuth alone
  would not stop it — a peer dials as a client too — so the server
  refuses that OU on `/RaftTransport/`, on both the unary and the
  streaming interceptor. Raft's transport is streaming; a unary-only
  guard would cover nothing that matters.

Enforced at the **server**. A check on the client is a check the
attacker controls.

An unidentified caller is refused on the peer-only path rather than
admitted. mTLS is mandatory on that listener, so a call with no
verified chain is not a configuration this cluster has — and guessing
in favour of the caller is the wrong way to be wrong about consensus.

Operator certificates are shorter-lived than a node's by default: a
person's credential lives on a laptop that travels.
