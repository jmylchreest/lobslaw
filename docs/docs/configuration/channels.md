---
sidebar_position: 5
---

# Channels

A **channel** is a way for users to talk to lobslaw. Telegram is the most-tested; REST + webhooks ship; Slack/Discord don't yet.

## Telegram

```toml
[[gateway.channels]]
type      = "telegram"
token_ref = "env:TELEGRAM_BOT_TOKEN"

[gateway.channels.user_scopes]
"123456789" = "owner"          # specific chat_id → scope override
"987654321" = "household"
```

The bot uses Telegram's long-poll `getUpdates` (no webhook). Egress role: `gateway/telegram` → `api.telegram.org` only.

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
  "user_id": "alice",
  "text": "what's on my calendar tomorrow?",
  "stream": false
}
```

```http
HTTP/1.1 200 OK

{ "reply": "...", "tool_calls": [...] }
```

Streaming (`"stream": true`) returns NDJSON events.

REST has **no async push** — replies are request/response. Use Telegram (or wire a webhook) for push notifications.

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

Without explicit scope binding, users default to `policy.unknown_user_scope` (recommended: `public`). Scopes:

- `owner` — you, the operator. Sensitive built-ins are typically allowed for this scope.
- `household` — trusted family members. Allow read tools + maybe scheduling.
- `public` — strangers. Allow `current_time` and not much else.

Bind a chat_id to a scope per-channel (`gateway.channels.user_scopes`) or globally via `[[user]]`:

```toml
[[user]]
id           = "alice"
display_name = "Alice"
scope        = "owner"
timezone     = "Europe/London"

[[user.channels]]
type    = "telegram"
address = "123456789"
```

User prefs live in raft; once bound, the scope persists across restarts.

## Notification routing

When the agent (or a commitment, or a research task) calls `notify(text="...")`:

- **Inbound originator known** → reply on the same channel (Telegram chat_id).
- **Self-generated (commitment, research, scheduled task)** → broadcast to every channel bound to `CreatedFor` user.
- **TTL** — transient notifications expire after 5 minutes if not delivered (channel offline, etc.).

REST channel returns an error on async push — that's correct behaviour, not a bug.

## Reference

- `internal/gateway/telegram.go` — long-poll loop, attachment download
- `internal/gateway/rest.go` — REST handler + auth
- `internal/gateway/webhook.go` — webhook HMAC verifier
- `internal/notify/` — channel-agnostic dispatch
- `pkg/config/config.go` — `GatewayConfig`, `ChannelConfig`
