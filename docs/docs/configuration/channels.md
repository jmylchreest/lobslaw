---
sidebar_position: 5
---

# Channels

A **channel** is a way for users to talk to lobslaw. Telegram is the most-tested; Slack, REST and webhooks ship; Discord and Matrix don't yet.

:::warning The allowlist is the gate, not `[[user]]`
Every channel authorises inbound messages from its **own** `user_scopes` map. `[[user]]` binds identity, timezone and roles — it grants **no** access. A channel with an empty `user_scopes` and an empty `gateway.unknown_user_scope` silently drops every message, with no reply and only a log line. That looks identical to a broken connection.
:::

## Telegram

```toml
[[gateway.channels]]
type          = "telegram"
bot_token_ref = "env:TELEGRAM_BOT_TOKEN"
mode          = "poll"
user_scopes   = { "123456789" = "owner", "987654321" = "household" }
```

`mode = "poll"` uses Telegram's long-poll `getUpdates` and needs no inbound network — the right default behind NAT. The alternative, `mode = "webhook"`, additionally requires `secret_token_ref`; leaving both unset fails at boot.

Egress role: `gateway/telegram` → `api.telegram.org` only.

Group chats are treated as **shared conversations**, which narrows what passive recall may surface into them — see [Slack → shared conversations](#shared-conversations) for the rule, which applies to both channels.

## Slack

```toml
[[gateway.channels]]
type          = "slack"
bot_token_ref = "env:SLACK_BOT_TOKEN"   # xoxb-…
app_token_ref = "env:SLACK_APP_TOKEN"   # xapp-…, needs connections:write

allowed_channels = ["dm", "C0123ABC"]         # "dm" = every DM; ["*"] = anywhere it is invited
user_scopes      = { "U06DZJWNACV" = "owner" }
```

Two tokens with different jobs: the **bot** token signs every Web API call, the **app** token opens the Socket Mode connection. Neither substitutes for the other, and both are required at boot.

Socket Mode is an **outbound** WebSocket, so this channel needs no public ingress, no Request URL and no request-signature verification. Egress role: `gateway/slack` → `slack.com` and `*.slack.com` (the wildcard is unavoidable — `apps.connections.open` picks a per-connection WSS subdomain).

The loop is pinned to the raft leader. Slack delivers each event to exactly one open connection, so two connected nodes would split a conversation between them at random.

### Slack app setup

In api.slack.com/apps:

1. **Socket Mode** → enable.
2. **Event Subscriptions** → enable, then *Subscribe to bot events*: `app_mention`, `message.im`, `message.channels`, `message.groups`, `message.mpim`.
3. **App Home** → enable the Messages Tab and tick *"Allow users to send Slash commands and messages from the messages tab"*. Without this the DM box is greyed out and `message.im` never fires.
4. **Slash Commands** → create `/lobslaw` (leave the Request URL blank). One umbrella command covers every command; see [Commands](#commands).
5. **Interactivity & Shortcuts** → enable (leave the Request URL blank — Socket Mode carries the payloads). Without this Slack never sends `block_actions`, so the buttons on a confirmation render but tapping them does nothing at all: no approval is recorded and the paused turn waits until it times out into a deny.
6. Invite the bot to any channel you want it in: `/invite @yourbot`.

Bot scopes needed: `app_mentions:read`, `chat:write`, `commands`, `users:read`, plus `channels:history`/`groups:history`/`im:history`/`mpim:history` for the conversations you want readable.

Granting `chat:write.public` is **not** recommended: without it the bot cannot post to a channel it was never invited to, which is a useful floor under the allowlist.

### `allowed_channels`

Empty is **closed**. An operator who has not said where the bot may act has not thereby said "anywhere".

It is enforced in two places, and both matter:

- on inbound events, deciding which conversations produce turns;
- inside `slack_read_channel` / `slack_search`, deciding which conversations the agent may fetch.

Enforcing only the first would govern what the agent *hears* while leaving what it can *go and read* wide open.

Three forms of entry:

| Entry | Matches |
|---|---|
| `"C0123ABC"` | that conversation, by id |
| `"dm"` | every direct message |
| `"*"` | every conversation |

`"dm"` exists because a DM's id is minted per user on first contact, so there is nothing to write down in advance. Without it, the only configuration that let anyone DM the bot was `["*"]` — which also opened every channel it had been invited to. The shape most deployments want is:

```toml
allowed_channels = ["dm", "C0123ABC"]   # anyone may DM it; it speaks in one channel
```

### Slash commands

A message matching `/<name>` is dispatched as a command **only when that command is registered**. Anything else — `/start`, which every Telegram client sends automatically on first contact, or `/help me pick a model` — falls through to the agent as an ordinary message.

That fall-through is deliberate. Answering every unregistered `/word` with *"Unknown command"* means the first thing a new Telegram user hears from the bot is a complaint about something they never typed.

### Threads and DMs

A thread is its own conversation, keyed `<channel>/<thread_ts>`, so each thread carries its own transcript and memory rather than interleaving into the channel's. Replies go into a thread in channels and inline in DMs.

### Shared conversations {#shared-conversations}

Anything that is not a 1:1 DM — a Slack channel, group DM, or Telegram group — is a **shared conversation**, and passive recall is narrowed there: the agent surfaces only memories the **speaker** owns, plus memories that **this conversation** produced.

Without that rule the speaker changes between turns and recall keyed on them alone would surface whatever the last person to type happens to own, to an audience that never owned any of it. DMs are unaffected.

An unknown channel type counts as shared. The two ways to be wrong are not symmetric: under-sharing costs some recall, over-sharing discloses one person's memories to a room.

The rule is **not** limited to passive recall. `memory_search` and the other `memory_*` builtins go through the same audience filter, so an explicit search in a shared conversation returns records other people own if this conversation produced them. That is the consistent choice — a rule that applied to background recall but not to the tool the model can simply call would not be a rule — but it is worth knowing before you ask the bot to search in a busy channel.

### Reading Slack as a source

`slack_read_channel` and `slack_search` let the agent read history. They are the only builtins with **no default-allow policy seed** — reading a workspace's conversations is an operator decision, so it takes an explicit rule:

There is **no per-user membership check**. `allowed_channels` is operator-global, so anyone who gets past the tool policy can read any allowed channel from a DM, whether or not they are a member of it. That is a much coarser rule than the shared-conversation rule above, and it is the doorway to the same content that rule is careful about — so scope the policy by subject, and list conversations rather than reaching for `"*"`.

```toml
[[policy.rules]]
id       = "owner-slack-read"
subject  = "scope:owner"
action   = "tool:exec"
resource = "slack_*"
effect   = "allow"
priority = 20
```

`slack_search` is a bounded local scan over recent history, not a workspace search: Slack's `search.messages` needs `search:read`, a user-token scope a bot cannot hold. Treat a miss as "not in recent history", never as "never said".

## Commands {#commands}

Slash commands share one dispatcher across channels, so `/new` means the same thing everywhere. Built in: `help`, `whoami`, `status`, `new`.

Every command is evaluated by the policy engine under action `command:exec` with the command name as the resource, and is **default-deny** — the builtin default-allow seed covers tool paths only:

```toml
[[policy.rules]]
id       = "operator-commands"
subject  = "role:operator"
action   = "command:exec"
resource = "*"
effect   = "allow"
priority = 20
```

On Slack these arrive as `/lobslaw <command>`; a directly-registered `/status` also dispatches as itself. Replies are ephemeral — only the person who ran it sees the output. `whoami` refuses to run outside a DM, since it prints your principal, scope and roles.

**File attachments** are downloaded to `/workspace/incoming/<turn_id>/` and surfaced to the agent's prompt as `[user attached: <local-path>]`. The agent can then call vision / audio / pdf builtins on the local path.

## REST

```toml
[[gateway.channels]]
type          = "rest"
listen        = ":8443"
require_auth  = true
jwt_validator = "google"
```

Speaks the standard agent shape:

```http
POST /v1/messages
Authorization: Bearer <jwt>
Content-Type: application/json

{
  "message": "what's on my calendar tomorrow?",
  "session_id": "daily-chat"
}
```

```http
HTTP/1.1 200 OK

{ "reply": "...", "tool_calls": [...] }
```

Streaming uses `Accept: text/event-stream` and returns server-sent events (SSE).

REST has **no async push** — replies are request/response. Use Telegram (or wire a webhook) for push notifications.

### Images and recorded speech over REST

Upload the raw image or audio body first, then include the returned `upload_id`
in a message. The upload endpoint streams to disk; file bytes do not go into the
message JSON or its existing 1 MiB request limit.

```sh
curl --fail-with-body "$GATEWAY/v1/uploads" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: image/png' \
  --data-binary @photo.png
# {"upload_id":"upload-...","mime_type":"image/png","size":12345,"expires_at":"..."}

curl --fail-with-body "$GATEWAY/v1/messages" \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  --data '{"message":"What is in this picture?","upload_ids":["upload-..."],"session_id":"daily-chat"}'
```

A browser can send a `File` or recorded audio `Blob` directly as the fetch body;
use its supported MIME type in `Content-Type`. Do not wrap it in `FormData` or
base64. For audio, use a prompt such as "Transcribe this recording". A message
may omit `message` when it supplies uploads. Vision and speech recognition use
the existing `read_image` / `read_audio` tools and their configured providers;
uploading a file does not itself enable those capabilities.

Supported media types: `image/png`, `image/jpeg`, `image/gif`, `image/webp`,
`audio/ogg`, `audio/webm`, `audio/mpeg`, `audio/mp4`, `audio/wav`, `audio/x-wav`,
and `audio/flac`. MIME parameters such as `audio/webm;codecs=opus` are accepted.
The media tool validates the actual file; declaring a MIME type does not convert
or validate the recording's codec.

- Maximum file size: **32 MiB**, checked while reading, including chunked uploads.
- Maximum staging capacity: **256 MiB and 128 files per gateway process**.
  In-progress uploads reserve 32 MiB each; capacity exhaustion returns HTTP 429.
- Up to **16 upload IDs per message**. Unknown, expired, duplicate or another
  user's IDs return HTTP 404. Oversized uploads return 413; unsupported types 415.
- Uploads expire after **one hour**. Files in use by a queued or active turn are
  retained until that request finishes, including its confirmation wait.
- IDs are bound to the authenticated user. With authentication disabled, callers
  share the existing anonymous identity; that mode provides no user isolation.
- Files live in a private temporary subdirectory of `[gateway].incoming_dir`.
  They are removed on expiry or graceful shutdown. After an abrupt process kill,
  an operator may remove abandoned `rest-uploads-*` directories once the owning
  process has stopped. Never remove directories used by a running gateway.

Upload and message requests must reach the **same gateway process** (use session
stickiness with multiple replicas). IDs do not survive restart and are not
replicated or backed up. Re-upload expired files for later conversation turns.
Media turns remain separate under `debounce` and `smart` queue modes; `latest`
and `off` still return an explicit rejection when they discard a request.

### JWT validators

```toml
[gateway.jwt_validators.google]
type    = "jwks"
jwks_url = "https://www.googleapis.com/oauth2/v3/certs"
issuer  = "https://accounts.google.com"

[gateway.jwt_validators.cloudflare-access]
type    = "jwks"
jwks_url = "https://<team>.cloudflareaccess.com/cdn-cgi/access/certs"
audience = "<application-aud>"
```

The validator pulls JWKS, verifies signature, extracts standard claims (sub, scope, iss, aud), maps to lobslaw's `Claims` struct via `gateway.user_scopes` overrides.

## Webhooks (inbound)

```toml
[[gateway.channels]]
type   = "webhook"
listen = ":8444"
path   = "/hooks"
secret_ref = "env:WEBHOOK_HMAC_SECRET"
```

External services POST to `https://<host>:8444/hooks`. The body is HMAC-verified, then queued as if a user sent it from a `webhook` channel. Useful for: GitHub push events, Stripe webhooks, IoT triggers.

## User scopes

Without explicit scope binding, users fall through to `gateway.unknown_user_scope` (recommended: `public`, or empty to reject). Scopes:

- `owner` — you, the operator. Sensitive built-ins are typically allowed for this scope.
- `household` — trusted family members. Allow read tools + maybe scheduling.
- `public` — strangers. Allow `current_time` and not much else.

**Scope comes from the channel's `user_scopes` map, and only from there.** `[[user]]` does something different and complementary: it binds a channel address to a canonical principal, and declares the policy roles that person holds. Both are usually needed.

```toml
# Authorisation: may this person talk to the bot, and as what?
[[gateway.channels]]
type        = "telegram"
user_scopes = { "123456789" = "owner" }

# Identity: who is this person, across channels?
[[user]]
id           = "alice"
display_name = "Alice"
timezone     = "Europe/London"
roles        = ["operator"]

[[user.channels]]
type    = "telegram"
address = "123456789"

# Roles only reach a channel with no JWT via an alias. The key is the
# channel-DERIVED id ("tg-<id>", "slack-<team>-<user>"), not the bare
# address above.
[identity.aliases]
"tg-123456789" = "alice"
```

There is no `scope` key on `[[user]]`; a scope written there is silently ignored. `lobslaw doctor` checks the alias and role wiring and names what is missing.

User prefs live in raft; once bound, they persist across restarts.

## Notification routing

When the agent (or a commitment, or a research task) calls `notify(text="...")`:

- **Inbound originator known** → reply on the same channel (Telegram chat_id).
- **Self-generated (commitment, research, scheduled task)** → broadcast to every channel bound to `CreatedFor` user.
- **TTL** — transient notifications expire after 5 minutes if not delivered (channel offline, etc.).

REST channel returns an error on async push — that's correct behaviour, not a bug.

## Reference

- `internal/gateway/telegram.go` — long-poll loop, attachment download
- `internal/gateway/slack.go` — Socket Mode loop, event routing, authorisation
- `internal/gateway/slack_read.go` — `slack_read_channel` / `slack_search` backing
- `internal/gateway/commands.go` — channel-agnostic slash-command dispatcher
- `internal/gateway/rest.go` — REST handler + auth
- `internal/gateway/webhook.go` — webhook HMAC verifier
- `internal/memory/visibility.go` — `Audience`, including the shared-conversation rule
- `internal/notify/` — channel-agnostic dispatch
- `pkg/config/config.go` — `GatewayConfig`, `GatewayChannelConfig`
