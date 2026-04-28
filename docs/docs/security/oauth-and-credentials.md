---
sidebar_position: 7
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

Every time a skill subprocess spawns:

```go
// internal/skills/invoker.go
cred, err := credentialsService.Get(ctx, claims, role)
if cred.RefreshToken != "" && time.Until(cred.ExpiresAt) < 5*time.Minute {
    cred, err = oauth.Refresh(ctx, provider, cred)
    credentialsService.Put(ctx, cred)  // writes raft entry
}
env = append(env, fmt.Sprintf("LOBSLAW_CRED_%s_TOKEN=%s", role, cred.AccessToken))
```

The skill never sees the refresh token, only the (short-lived) access token. It can't persist; it can't refresh on its own.

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
- **`cred refresh failed: invalid_grant`** — refresh token revoked (user re-authed elsewhere, password changed, security review). Re-run the device flow.
- **Skill error: "no credential available"** — operator hasn't granted access. Run `credentials_grant`.
- **Skill works once, then 401s** — operator's clock is wildly off and the access token "expires" before refresh logic kicks in. Run NTP.

## Reference

- `internal/oauth/` — provider modules, flows, refresh, userinfo
- `internal/memory/credentials.go` — encrypted bucket
- `internal/compute/builtin_credentials.go` — operator-facing tools
- `internal/skills/invoker.go` — pre-spawn token refresh
- `pkg/crypto/seal.go` — AEAD wrapper
- `pkg/proto/lobslaw/v1/lobslaw.proto` — `CredentialRecord`, `CredentialACL`
