---
sidebar_position: 10
---

# OAuth and Credentials

How the cluster takes the operator from "I have a Google account" to "skill subprocesses see fresh access tokens" without any token ever touching disk in plaintext.

## Flow overview

```
   ┌────────────────┐
   │ user (in chat) │  "start oauth flow for google"
   └───────┬────────┘
           │
           ▼
   ┌─────────────────┐
   │ oauth_start     │  policy-gated, scope:owner
   │  (builtin)      │
   └───────┬─────────┘
           │ POST device_authorization_endpoint
           ▼
   ┌─────────────────┐    user opens URL,
   │ device flow     │    enters code,
   │ (RFC 8628)      │    approves
   └───────┬─────────┘
           │
           ▼ background poller
   ┌─────────────────┐
   │ token endpoint  │ ─► access_token + refresh_token
   └───────┬─────────┘
           │
           ▼ /userinfo to resolve subject
   ┌─────────────────┐
   │ FetchSubject    │ ─► google:1234567890
   └───────┬─────────┘
           │
           ▼  Seal(MemoryKey, payload)
   ┌─────────────────┐
   │ credentials     │  raft-replicated, encrypted at rest
   │   bucket        │
   └─────────────────┘
```

## The four moving parts

### 1. OAuth providers (`internal/oauth/`)

One file per provider, each implementing the same `Provider` interface:

- `provider_github.go`
- `provider_gitlab.go`
- `provider_google.go`
- `provider_microsoft.go`

A provider declares its device-authorization, token, and userinfo endpoints. Adding a new provider is a copy-paste-plus-endpoints job.

### 2. Device flow (`internal/oauth/flows.go`)

Implements [RFC 8628](https://www.rfc-editor.org/rfc/rfc8628) — the OAuth 2.0 Device Authorization Grant. The user gets a URL + short code; the cluster's background poller asks the token endpoint every `interval` seconds until the user approves (or denies, or expires).

This is what makes lobslaw usable from a phone-only Telegram session — no redirect URI, no localhost listener, no "paste this back into the terminal".

### 3. Subject resolution (`internal/oauth/userinfo.go`)

After grant, the poller calls the provider's `/userinfo` (or equivalent) to fetch a stable subject identifier. The credential is keyed by `<provider>:<subject>` — so even if the user has multiple Google accounts, each one is a distinct credential.

### 4. Encrypted-at-rest storage (`internal/memory/credentials.go`)

Credentials live in a dedicated bolt bucket, raft-replicated. The payload (access token, refresh token, expiry, scopes) is sealed with `crypto.Seal(memoryKey, payload)` — AES-256-GCM with a per-record nonce.

The MemoryKey is provisioned at cluster bootstrap (currently via `LOBSLAW_MEMORY_KEY` env var) and is identical on every peer. A peer that's compromised has read access to the credential plaintext; that's the explicit trust model. See [Threat model](/security/threat-model).

## Refresh on spawn

`CredentialService.IssueForSkill` checks the skill's grant and refreshes tokens
that expire within 60 seconds. Before contacting the provider, it acquires a
revision-checked claim through Raft for that provider and subject. Other callers,
including callers on other nodes, wait for the result; unrelated credentials
can refresh independently. Each caller's permissions are checked again against
the refreshed record, and returned scopes are limited to those still granted
by both the operator and provider.

The refresh has a 30-second context deadline and a separate 10-second persistence
budget. If the initiating caller disconnects, the refresh continues so a rotated
token is not discarded. Grant/revoke operations use conditional writes; refresh
completion preserves their changes. Deleting or reconnecting an account prevents
an older in-flight refresh from overwriting it.

A refresh error, lost response, or crashed refresher can leave the provider's
rotation outcome uncertain. The durable attempt is **not** automatically taken
over when its deadline passes: resending an already-consumed token could revoke
the replacement. The call reports `refresh outcome uncertain; reconnect the
account`. Re-run `oauth_start` and restore the required skill grants. The current
refresher interface cannot distinguish safely retryable provider errors from
uncertain outcomes, so errors are conservatively treated this way. An abandoned
claim remains visible after restart; a successful new authorization replaces it.

Upgrade every node that applies credential writes or serves credential requests
before resuming OAuth credential operations. Older binaries do not enforce the
new claim fields. No new token-encryption format or external service is required.

The skill receives only the short-lived access token, never the refresh token.

## Granting skills access

Credentials are *separate* from skills. A skill must be explicitly granted access:

```
> grant gws-workspace access to my google credential, scopes gmail.readonly and calendar.readonly
```

Behind the scenes:

1. `credentials_grant` builtin (operator-only) writes a `CredentialACL` entry: skill `gws-workspace` ↔ credential `google:1234567890` ↔ scopes `[gmail.readonly, calendar.readonly]`.
2. On spawn, the invoker checks the ACL; if absent or insufficient scope, the skill spawns *without* the credential env var. The skill detects the missing var and (typically) returns an error to the agent.

This separation is deliberate: installing a skill doesn't grant it access to anything. The operator holds the credential ↔ skill mapping.

## OAuth provider config

```toml
[security.oauth.google]
client_id_ref     = "env:GOOGLE_OAUTH_CLIENT_ID"
client_secret_ref = "env:GOOGLE_OAUTH_CLIENT_SECRET"
# device_auth_endpoint, token_endpoint, userinfo_endpoint default to the
# provider defaults; override only for self-hosted IdPs

[security.oauth.github]
client_id_ref     = "env:GITHUB_OAUTH_CLIENT_ID"
client_secret_ref = "env:GITHUB_OAUTH_CLIENT_SECRET"
```

`*_ref` lets the secret live in `.env`, a Vault sidecar, or anywhere that resolves at boot — never in `config.toml` directly.

## Common pitfalls

- **`oauth_start: provider "google" not configured`** — missing `[security.oauth.google]` block.
- **`refresh outcome uncertain; reconnect the account`** — a refresh failed or its completion could not be confirmed. Re-run the device flow; the old token will not be retried automatically.
- **Skill error: "no credential available"** — operator hasn't granted access. Run `credentials_grant`.
- **Skill works once, then 401s** — operator's clock is wildly off and the access token "expires" before refresh logic kicks in. Run NTP.

## Reference

- `internal/oauth/` — provider modules, flows, refresh, userinfo
- `internal/memory/credentials.go` — encrypted bucket
- `internal/compute/builtin_credentials.go` — operator-facing tools
- `internal/skills/invoker.go` — pre-spawn token refresh
- `pkg/crypto/seal.go` — AEAD wrapper
- `pkg/proto/lobslaw/v1/lobslaw.proto` — `CredentialRecord`, `CredentialACL`
