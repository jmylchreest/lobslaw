---
sidebar_position: 2
---

# Reference

Full TOML reference. Every key, type, default. The authoritative source is `pkg/config/config.go` — this page mirrors it.

## `[cluster]`

```toml
[cluster]
node_id        = "node-1"           # required; lobslaw nodeid for default
listen_address = "0.0.0.0:7000"     # raft + intra-cluster gRPC
gateway_port   = 8443               # gateway TLS listen
peers          = ["node-2:7000", "node-3:7000"]   # static peer list

[cluster.mtls]
ca_cert   = "certs/ca.pem"
node_cert = "certs/node.pem"
node_key  = "certs/node-key.pem"
```

## `[memory]`

```toml
[memory]
data_dir = "data"

[memory.encryption]
key_ref = "env:LOBSLAW_MEMORY_KEY"   # base64 32-byte key

[memory.snapshot]
interval         = "1h"
trailing_logs    = 10000
threshold        = 8192
```

## `[storage]`

```toml
[storage]
default_mount = "workspace"

[[storage.mounts]]
label = "workspace"
type  = "local"
path  = "/workspace"
mode  = "rw"

[[storage.mounts]]
label = "skill-tools"
type  = "local"
path  = "/var/lib/lobslaw/skills"
mode  = "ro"
```

## `[security]`

```toml
[security]
egress_upstream_proxy        = ""
egress_allow_private_ranges  = false
egress_allow_ranges          = []
egress_uds_path              = ""             # required for netns skills
clawhub_base_url             = ""             # set to enable clawhub_install
clawhub_binary_hosts         = []             # default github.com release hosts
clawhub_install_mount        = "skill-tools"
clawhub_signing_policy       = "prefer"       # off | prefer | require
fetch_url_allow_hosts        = []             # empty = permissive

# Per-provider OAuth configuration
[security.oauth.google]
client_id_ref     = "env:GOOGLE_OAUTH_CLIENT_ID"
client_secret_ref = "env:GOOGLE_OAUTH_CLIENT_SECRET"
# device_auth_endpoint, token_endpoint, userinfo_endpoint default to provider standard

[security.oauth.github]
client_id_ref     = "env:GITHUB_OAUTH_CLIENT_ID"
client_secret_ref = "env:GITHUB_OAUTH_CLIENT_SECRET"
```

## `[policy]` + `[[policy.rules]]`

```toml
[policy]
enabled               = true
unknown_user_scope    = "public"      # NEVER set to anything else for prod

[[policy.rules]]
id          = "owner-soul-tools"
description = "Owner can mutate soul"
priority    = 20
effect      = "allow"
subject     = "scope:owner"
action      = "tool:exec"
resource    = "soul_*"
```

See [policy rules](/configuration/policy-rules) for matching semantics.

## `[compute]`

```toml
[compute]
default_provider = "openrouter"

[[compute.providers]]
label              = "openrouter"
endpoint           = "https://openrouter.ai/api/v1/chat/completions"
api_key_ref        = "env:OPENROUTER_API_KEY"
model              = "anthropic/claude-sonnet-4"
trust_tier         = "primary"        # primary | backup | adversarial
capabilities       = ["chat"]
auto_capabilities  = true             # opt-in to models.dev capability discovery
backup             = "openrouter-fallback"

[compute.embeddings]
endpoint    = "https://openrouter.ai/api/v1/embeddings"
api_key_ref = "env:OPENROUTER_API_KEY"
model       = "openai/text-embedding-3-small"
dimensions  = 1536

[compute.roles]
worker = "openrouter"
council = ["openrouter", "anthropic-direct"]

[compute.web_search]
provider = "tavily"
api_key_ref = "env:TAVILY_API_KEY"

[compute.limits]
max_tool_calls_per_turn = 25
max_turn_seconds        = 600
```

## `[gateway]`

```toml
[gateway]
require_auth        = false
default_scope       = "public"
unknown_user_scope  = "public"

[[gateway.channels]]
type      = "telegram"
token_ref = "env:TELEGRAM_BOT_TOKEN"

[gateway.channels.user_scopes]
"123456789" = "owner"        # chat_id → scope override

[[gateway.channels]]
type      = "rest"
listen    = ":8443"
jwt_validator = "google"
```

## `[mcp.servers.<name>]`

```toml
[mcp.servers.minimax]
command  = "uvx"
args     = ["minimax-mcp-server"]
env      = { MINIMAX_API_KEY = "ref:env:MINIMAX_API_KEY" }
networks = ["api.minimax.chat"]
```

## `[scheduler]`

```toml
[scheduler]
storage = "raft"             # raft | local
tick_interval = "1m"
```

## `[skills]`

```toml
[skills]
discover_paths = ["/var/lib/lobslaw/skills"]
default_signing_policy = "prefer"
```

## `[soul]`

```toml
[soul]
path = "SOUL.md"
fragments = 16
dream_interval = "24h"
```

## `[audit.local]`

```toml
[audit.local]
path = "audit/audit-{date}.jsonl"
mode = 0640
```

## Other sections

`[discovery]`, `[observability]`, `[hooks]`, `[users]` — see `pkg/config/config.go` for the full schema. These are stable but rarely-touched.

## Secret references

Anywhere the schema says `*_ref`:

```
"env:NAME"          # read from environment
"file:/path"        # read from a file (chmod 0600 strongly recommended)
"vault:secret/..."  # planned; not wired yet
"literal:foo"       # inline (testing only — do not use in prod)
```

## Hot reload

`SIGHUP` reloads `config.toml`:

- Policy rules — applied to next tool call
- Egress ACL — applied to next CONNECT
- mTLS certs — atomic swap, in-flight handshakes unaffected
- Provider list — applied to next LLM call
- Channels — channel-specific; Telegram restart its long-poll, REST closes its listener and re-binds

Anything else (raft listen address, mount paths, encryption key) requires a process restart.
