---
sidebar_position: 2
---

# Reference

Full TOML reference. Every key, type, default. The authoritative source is `pkg/config/config.go` — this page mirrors it.

## `[cluster]`

```toml
[cluster]
listen_addr    = "0.0.0.0:7443"     # raft + intra-cluster gRPC
advertise_addr = "10.0.0.4:7443"    # what peers dial; derived from listen_addr
data_dir       = "data"             # raft log + state.db + snapshots
bootstrap      = true               # form a cluster alone; false requires a seed

[cluster.mtls]
ca_cert   = "certs/ca.pem"
node_cert = "certs/node.pem"
node_key  = "certs/node-key.pem"

# Operator enrolment. The operator CA signs PEOPLE and is online; the
# cluster CA above signs NODES and stays offline. Separate keys on
# purpose — see /security/operator-credentials.
operator_ca_cert = "certs/operator-ca.pem"    # default: beside ca_cert
operator_ca_key  = "certs/operator-ca-key.pem" # created on first use
enrol_addr       = ":9091"                     # empty disables enrolment
enrol_valid_for  = "2160h"                     # 90d default
```

There is no `node_id` key. The node identity comes from `$LOBSLAW_NODE_ID`, or the
short hostname, or a random fallback — `lobslaw nodeid` prints what this host
would resolve to.

Peers are `[discovery] seed_nodes`, and the gateway port is `[gateway] http_port`.

`enrol_addr` is a **separate listener** that requires no client certificate,
because a laptop enrolling does not have one yet. It serves only the submit and
poll RPCs. Leaving it empty disables new enrolments; credentials already issued
keep working.

## `[memory]`

```toml
[memory]
data_dir = "data"

[memory.encryption]
key_ref = "env:LOBSLAW_MEMORY_KEY"   # base64 32-byte key

[memory.snapshot]
# Boot validation only today: a memory node with neither a snapshot
# target nor seed nodes is a single point of loss. Shipping is not
# implemented, so there is no schedule or retention to configure.
target = "local"
```

## `[storage]`

```toml
[storage]
enabled = true

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
# Skill signing policy lives in [skills], not here.
clawhub_base_url             = "https://clawhub.dev" | require
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
default_chain = "main"

[[compute.providers]]
label              = "openrouter"
driver             = "openai"         # openai (default) | anthropic | mock
endpoint           = "https://openrouter.ai/api/v1/chat/completions"
api_key_ref        = "env:OPENROUTER_API_KEY"
model              = "anthropic/claude-sonnet-4"
trust_tier         = "public"         # public | private | local, or 1-100 (higher = more trusted)
capabilities       = ["chat"]
auto_capabilities  = true             # opt-in to models.dev capability discovery
backup             = "openrouter-fallback"

# `driver` names the WIRE PROTOCOL, not the vendor. Qwen Cloud, Groq,
# Together, Fireworks and a local Ollama are all `driver = "openai"`
# with different endpoints — which is why the driver list stays short
# while the provider list does not. Omitting it means "openai".
#
# Use `driver = "anthropic"` to talk to the Messages API directly
# rather than through an OpenAI-compatible relay. It sends x-api-key
# rather than a bearer token; api_key_ref is unchanged.
[[compute.providers]]
label       = "claude-direct"
driver      = "anthropic"
model       = "claude-sonnet-4-6"
api_key_ref = "env:ANTHROPIC_API_KEY"
trust_tier  = "private"
capabilities = ["chat"]

# `driver = "mock"` answers without touching the network. For a node
# that must boot and serve turns offline — CI, a smoke test, or
# reproducing a bug without spending tokens.
[[compute.providers]]
label      = "offline"
driver     = "mock"
model      = "mock-main"
trust_tier = "local"

[compute.embeddings]
endpoint    = "https://openrouter.ai/api/v1/embeddings"
api_key_ref = "env:OPENROUTER_API_KEY"
model       = "openai/text-embedding-3-small"
dims        = 1536

[compute.roles]
# main, preflight, reranker, summariser. There is no "worker" or
# "council" role.
main = "openrouter"

[compute.web_search]
provider = "tavily"
api_key_ref = "env:TAVILY_API_KEY"

[compute.limits]
max_tool_calls_per_turn = 25

[compute.context]
# How much prior conversation each turn replays, and how the rest is
# compacted. Every value is optional; the defaults shown are applied
# when the key is absent. An explicit 0 DISABLES a bound rather than
# taking the default.
tail_tokens                   = 4000   # verbatim history budget per turn
history_tool_result_bytes     = 512    # truncate REPLAYED tool results
compact_enabled               = true   # false disables compaction
compact_keep_messages         = 40     # never summarise the recent exchange
compact_trigger_tokens        = 1500   # aged-out volume that justifies a call
compact_max_summary_tokens    = 600    # cap on the stored summary
compact_tool_result_bytes     = 400    # tool output the summariser reads
# Appended to the built-in summariser prompt, not replacing it.
compact_instructions = "Always keep ticket numbers and schema decisions."

# Conversation titles, generated once on first compaction.
titles_enabled = true

# `tail_messages`, `compact_max_completion_tokens` and
# `title_max_chars` used to be here. The first two were second caps on
# things another setting already capped, and the tighter of two caps
# wins silently — a `tail_messages` of 5 beside a generous
# `tail_tokens` truncated history for a reason nobody could see in the
# config. They are derived now, so the pair cannot disagree.
# `title_max_chars` was a UI constant: a title's length does not vary
# by deployment.

# Bounds on what the session_* tools may pull into the agent's
# context. Unbounded, one session_read undoes the context budget in a
# single tool call.
session_search_results  = 5
session_search_snippets = 3
session_read_messages   = 40
```

**Sizing them.** `tail_tokens` is the one to move first: it sets what a turn
costs. `compact_max_summary_tokens` must stay well below it — the summary is
prepended to every turn, and config validation rejects a summary budget larger
than the verbatim budget it is meant to make room for.

`compact_trigger_tokens` trades LLM calls against context freshness: lower means
more frequent, smaller summarisation calls; higher means aged-out messages sit
un-summarised for longer and may be dropped by `tail_tokens` before they are
ever folded in.

Compaction needs a summariser: set `[compute.roles].summariser` to a cheap model
so compaction doesn't run on your expensive one. With no summariser resolved,
compaction is off and long conversations simply lose their oldest messages.

## `[gateway]`

```toml
[gateway]
require_auth        = false
unknown_user_scope  = "public"
unknown_user_scope  = "public"

# What happens when a message arrives while a turn is already running
# for the same conversation. Turns on one conversation never overlap in
# any mode — the modes differ only in what becomes of the second
# message.
#
#   serial    queue behind the running turn, answer in arrival order
#             (default; nothing is dropped)
#   debounce  hold briefly and fold consecutive messages into one turn
#             — matches how people actually type
#   latest    keep only the newest queued message, discard what it
#             overtook
#   off       drop messages that arrive mid-turn, telling the user
#
# An unrecognised value is rejected at boot rather than defaulting, so
# a typo cannot silently change how messages are handled.
queue_mode     = "serial"
# Fold window for queue_mode = "debounce". Ignored otherwise. 0 → 3s.
queue_debounce = "3s"

[identity]
# Maps the per-channel user ids lobslaw receives onto cluster-wide
# principals. Only needed when one person reaches the node under more
# than one id — the usual case being the same human on Telegram and
# over REST.
#
# Without an entry, every channel id is its own principal. That is
# correct but strict: that person will not find their Telegram history
# from a REST session, and memories written on one channel are not
# visible from the other. Ownership and visibility are decided against
# the resolved principal, so this is also what decides whose memories
# passive recall may inject into a turn.
#
# Values are bare ids; lobslaw prefixes the principal kind itself.
# Keys match case-insensitively.
[identity.aliases]
# "tg-@alice"         = "alice"
# "alice@example.com" = "alice"

[memory.session]
# TTL after which a retention=session record becomes a prune candidate.
# Episodic turn ingest writes at this retention, so this is how long a
# conversation stays semantically searchable before Dream has to have
# consolidated anything worth keeping. Empty/zero → 24h.
max_age  = "24h"
# Cron for the auto-seeded prune task. Empty → "@hourly". The prune
# itself is cheap: a linear bucket scan plus one raft.Apply per stale
# record.
schedule = "@hourly"

# Conversation storage. session_max_messages is a STORAGE bound — it
# sets what is kept on disk for search and export, not what is sent to
# the model (that is [compute.context]).
session_max_messages  = 200
# The in-memory buffer that fronts the durable store. It is the
# degraded mode for turns handled on a raft follower, since session
# writes are leader-only — not a performance cache.
session_cache_messages = 100
session_cache_ttl      = "30m"

[[gateway.channels]]
type      = "telegram"
bot_token_ref = "env:TELEGRAM_BOT_TOKEN"

[gateway.channels.user_scopes]
"123456789" = "owner"        # chat_id → scope override

[[gateway.channels]]
# The REST listener is [gateway] http_port; a channel has no listen
# address of its own.
type      = "rest"
```

## `[mcp.servers.<name>]`

```toml
[mcp.servers.minimax]
command  = "uvx"
args     = ["minimax-mcp-server"]
env      = { MINIMAX_API_KEY = "ref:env:MINIMAX_API_KEY" }
```

## `[scheduler]`

```toml
```

## `[skills]`

```toml
[skills]
signing_policy = "prefer"
```

## `[soul]`

```toml
[soul]
path = "SOUL.md"
```

## `[audit.local]`

```toml
[audit.local]
path = "audit/audit-{date}.jsonl"
mode = 0640
```

## `[[user]]`

One entry per human the deployment knows about. Seeded into the
user-preferences bucket on first boot; runtime edits win on subsequent
boots, so this is the initial declaration rather than a live mirror.

```toml
[[user]]
id           = "alice"
display_name = "Alice"
timezone     = "Europe/London"
language     = "en"
roles        = ["operator"]

[[user.channels]]
type    = "telegram"
address = "123456789"
```

`id` is the canonical principal id — the same value `[identity.aliases]`
maps channel ids onto. Everything else is keyed off it.

### `roles`

Policy subjects this person holds, matched by rules written as
`subject = "role:operator"`. A JWT's `roles` claim does the same job for
REST callers; this key is how a channel with no token — Telegram — says
the same thing.

Roles are looked up by resolved principal, so `roles` on `id = "alice"`
covers every channel id `[identity.aliases]` maps to `alice`. A channel
id with no alias entry resolves to itself and therefore matches no
`[[user]]` block, which means an operator arriving on a second channel
needs an alias entry before their role follows them there.

**Holding a role grants nothing on its own.** It only makes the
principal matchable by a rule. Cross-owner memory access in particular
is a policy decision, not a property of the role:

```toml
[[policy.rules]]
id       = "operators-read-any-memory"
subject  = "role:operator"
action   = "memory:read:any"
resource = "memory:*"
effect   = "allow"
priority = 50
```

Nothing seeds that rule. Without it, a `role:operator` principal reads
exactly what they own, the same as anyone else. With it, every widened
read — `memory_search`, passive recall, a cross-owner `memory_forget` —
appends an entry to the audit chain naming the principal and the rule
that allowed it.

`effect = "deny"` and `effect = "require_confirmation"` both refuse the
widening. Confirmation is not offered: two of the three call sites run
with no user in front of them to ask, and an effect chosen to slow
something down must not become the one that speeds it up.

## Other sections

`[discovery]`, `[observability]`, `[hooks]` — see `pkg/config/config.go` for the full schema. These are stable but rarely-touched.

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
