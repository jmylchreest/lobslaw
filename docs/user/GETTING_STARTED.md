# Getting Started

End-to-end walkthrough for a first-time operator: from `lobslaw init` to a working agent that talks via Telegram, calls Google APIs through an installed skill, and proxies all egress through smokescreen.

This guide assumes a single host running Linux. Multi-node clusters use the same flow on each node — see [CHANNELS.md](CHANNELS.md) for cluster topology.

## Prerequisites

- Go 1.22+ (only if building from source — released binaries skip this).
- A Telegram bot token (talk to [@BotFather](https://t.me/BotFather)).
- An LLM provider API key. OpenRouter is the easy default — sign up at [openrouter.ai](https://openrouter.ai/) and grab a key.
- A Google Cloud OAuth client (only if using the Google Workspace skill). [Console → APIs & Services → Credentials → "TVs and Limited Input Devices"](https://console.cloud.google.com/apis/credentials).

## 1. `lobslaw init`

```bash
cd ~/.config/lobslaw    # or wherever you want the data dir
lobslaw init
```

Interactive prompts produce:

- `config.toml` — the main config.
- `.env` — your secrets (chmod 0600). Never commit this.
- `data/` — raft state, vector index, episodic memory.
- `audit/` — JSONL audit log.
- `certs/` — mTLS material (generated next).

## 2. mTLS bootstrap

```bash
lobslaw cluster ca-init --ca-cert certs/ca.pem --ca-key certs/ca-key.pem
lobslaw cluster sign-node --ca-cert certs/ca.pem --ca-key certs/ca-key.pem \
                          --node-cert certs/node.pem --node-key certs/node-key.pem \
                          --node-id $(lobslaw nodeid)
```

The CA private key is sensitive but stays on this host — single-node deployments don't need to distribute it. For multi-node, copy the CA certs to peer nodes and run `cluster sign-node` on each one.

## 3. Smokescreen + egress

The default config has smokescreen enabled with a permissive `fetch_url` role and no UDS. If you plan to install skills with `network_isolation: true`, set:

```toml
[security]
egress_uds_path = "/tmp/lobslaw-egress.sock"
```

This lets netns-isolated subprocesses dial the proxy. Skills without netns isolation use TCP loopback automatically.

## 4. Telegram channel

In `config.toml`:

```toml
[[gateway.channels]]
type = "telegram"
token_ref = "env:TELEGRAM_BOT_TOKEN"
```

Add to `.env`:

```sh
TELEGRAM_BOT_TOKEN=123456:abcdef-your-bot-token
```

## 5. Verify the config

```bash
lobslaw doctor --config config.toml
```

Expected output: every check passes. Fix anything `FAIL`-marked before booting — the most common surprises are file permissions on `.env` (must be 0600) and unreachable LLM endpoints from this host.

## 6. First boot

```bash
lobslaw --config config.toml
```

You should see lines like:

```
INFO  egress: smokescreen proxy started bind=127.0.0.1:NNNNN roles=8
INFO  policy: seeded default builtin rules count=15
INFO  raft leadership changed is_leader=true
INFO  lobslaw node started node_id=node-XXX
```

Send your bot a message in Telegram. The agent should reply.

## 7. OAuth + skill install (optional)

To use the Google Workspace skill, you need to (a) declare the OAuth provider, (b) authorize via the device-code flow, and (c) install + grant the skill.

### 7a. Declare the provider

```toml
[security.oauth.google]
client_id_ref     = "env:GOOGLE_OAUTH_CLIENT_ID"
client_secret_ref = "env:GOOGLE_OAUTH_CLIENT_SECRET"
```

`.env`:

```sh
GOOGLE_OAUTH_CLIENT_ID=NNNNN-XXXXXXXX.apps.googleusercontent.com
GOOGLE_OAUTH_CLIENT_SECRET=GOCSPX-XXXXXXXXXXXX
```

Reload (or restart the node).

### 7b. Authorize

In Telegram (as the operator scope):

> **You:** `start oauth flow for google`
>
> **Bot:** Started flow `01HXAB...`. Visit https://www.google.com/device and enter code `ABCD-EFGH`.

Visit the URL, enter the code, approve. The bot's background poller picks up the grant and persists the credential.

> **You:** `oauth status`
>
> **Bot:** 1 flow complete (google, alice@example.com, scopes: openid email profile)

### 7c. Install the skill

```bash
lobslaw plugin install clawhub:gws-workspace@1.0.0
```

(Requires `[security] clawhub_base_url` set; otherwise install from a local directory.)

### 7d. Grant the skill access to your credential

> **You:** `grant gws-workspace access to my google credential, scopes gmail.readonly and calendar.readonly`
>
> **Bot:** Granted.

### 7e. Use it

> **You:** `what's on my calendar tomorrow?`
>
> **Bot:** [calls gws-workspace skill, which reads $LOBSLAW_CRED_GOOGLE_TOKEN, queries Calendar API through the egress proxy, returns: ...]

## 8. Operator-only commands

By default, the agent's mutator builtins (`oauth_*`, `credentials_*`, `clawhub_install`, `soul_*`) are policy-default-deny. Open them per-scope from your operator chat:

```toml
[[policy.rules]]
id       = "owner-can-mutate-soul"
subject  = "owner"
action   = "tool:exec"
resource = "soul_tune"
effect   = "allow"
priority = 20
```

Repeat for every builtin you want the operator to call. Strangers in non-owner scopes still get denied.

## 9. Cert rotation

Send `SIGHUP` to the running process after replacing the cert files:

```bash
kill -HUP $(pidof lobslaw)
```

Atomic-swap; in-flight requests aren't disrupted, new TLS handshakes pick up the rotated material.

## 10. Container deploy

The Docker image at `deploy/docker/Dockerfile` includes the lobslaw binary. The recommended layout:

- Mount `config.toml` from a ConfigMap or bind-mount.
- Mount `.env` from a Secret with `mode: 0600`.
- Provide `certs/` from a Secret (or use a Vault sidecar that drops them at the expected paths).
- Persistent volume for `data/` so raft state survives pod restart.

A working `compose.yml` lives at [`deploy/docker/cluster.yml`](../../deploy/docker/cluster.yml).

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `FAIL .env readable + chmod 0600` | `chmod 0600 .env` |
| `LLM provider reachable: dial timeout` | Provider hostname unreachable from this host, or smokescreen ACL too strict |
| `oauth_start: provider "google" not configured` | Missing `[security.oauth.google]` block |
| `clawhub_install: not authorised` | Operator hasn't added an allow rule for `clawhub_install` |
| `network_isolation requested but no UDS configured` | Set `[security] egress_uds_path` |
| `cred refresh failed: invalid_grant` | Refresh token revoked — re-run oauth_start |

## Where to go next

- [`DESIGN.md`](../../DESIGN.md) — architecture overview.
- [`CHANNELS.md`](CHANNELS.md) — REST + webhook channel details.
- [`docs/dev/`](../dev/) — developer-side docs for hacking on the codebase.
