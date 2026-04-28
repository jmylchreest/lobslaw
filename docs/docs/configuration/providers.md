---
sidebar_position: 4
---

# Providers

The LLM provider router. Each `[[compute.providers]]` block declares one upstream model.

## Minimum

```toml
[[compute.providers]]
label       = "openrouter"
endpoint    = "https://openrouter.ai/api/v1/chat/completions"
api_key_ref = "env:OPENROUTER_API_KEY"
model       = "anthropic/claude-sonnet-4"
```

That's enough to get a working chat agent.

## Capability declaration

```toml
[[compute.providers]]
label             = "minimax"
endpoint          = "https://api.minimax.chat/v1/text/chatcompletion_v2"
api_key_ref       = "env:MINIMAX_API_KEY"
model             = "MiniMax-M2"
capabilities      = ["chat"]                     # explicit: text-only
auto_capabilities = false
```

Capabilities determine which builtins route to this provider:

- `chat` — agent loop default
- `vision` — `read_image` builtin
- `audio-transcription` — `read_audio` builtin
- `pdf` — `read_pdf` builtin
- `embedding` — embedder
- `function-calling` — tool-using turns

The agent uses the most-trusted provider that has the required capability. If your council needs vision, every provider in `[compute.roles] council` either declares `capabilities = ["vision"]` or you explicitly pin per-role:

```toml
[compute.roles]
vision = "openrouter"   # send vision turns through openrouter regardless of default
```

## models.dev auto-discovery

```toml
[[compute.providers]]
label             = "openrouter"
endpoint          = "https://openrouter.ai/api/v1/chat/completions"
api_key_ref       = "env:OPENROUTER_API_KEY"
model             = "anthropic/claude-sonnet-4"
auto_capabilities = true
```

When `auto_capabilities = true`, lobslaw fetches models.dev's catalog at boot and intersects all entries for that model. **Declared capabilities always win on conflict** — operator authority is preserved.

Conservative discovery: when a model name appears in multiple provider listings, the **intersection** is taken — only claim a capability when every catalog entry agrees. This guards against catalog bugs (e.g. one source incorrectly tagging MiniMax-M2 as multimodal when every other source listed it correctly as text-only).

## Trust tiers

```toml
[[compute.providers]]
label      = "openrouter"
trust_tier = "primary"        # primary | backup | adversarial
backup     = "openrouter-fallback"
```

| Tier | Used by | Fallback |
|---|---|---|
| `primary` | default chat, scheduled tasks, commitments | falls back to `backup` provider on rate-limit / 5xx |
| `backup` | failover only | usually not called directly |
| `adversarial` | `council_review(mode="adversarial")` | independent — never falls back |

The `adversarial` tier is for council reviews — you want a *different* provider's opinion, so a fallback to the primary defeats the purpose.

## Endpoints

`endpoint` should be the full URL up to and including the chat completions path. Common forms:

| Provider | Endpoint |
|---|---|
| OpenRouter | `https://openrouter.ai/api/v1/chat/completions` |
| Anthropic | `https://api.anthropic.com/v1/messages` |
| OpenAI | `https://api.openai.com/v1/chat/completions` |
| MiniMax | `https://api.minimax.chat/v1/text/chatcompletion_v2` |
| Local Ollama | `http://localhost:11434/v1/chat/completions` (requires `egress_allow_private_ranges = true`) |
| vLLM / TGI | whatever you wired |

The endpoint hostname is automatically added to the `llm` egress role's allowlist.

## Embeddings

```toml
[compute.embeddings]
endpoint    = "https://openrouter.ai/api/v1/embeddings"
api_key_ref = "env:OPENROUTER_API_KEY"
model       = "openai/text-embedding-3-small"
dimensions  = 1536
```

The embedder is used for episodic memory recall, semantic search, and dream synthesis. Match `dimensions` to your model — mismatches will silently produce garbage results.

If you change embedding model after data already exists in the index, run:

```bash
lobslaw backfill-embeddings --config config.toml
```

This re-embeds every record with the new model. Without it, recall quality collapses.

## Roles

```toml
[compute.roles]
worker  = "openrouter"
council = ["openrouter", "anthropic-direct", "minimax"]
vision  = "openrouter"
```

| Role | Used by |
|---|---|
| `worker` | individual research workers (parallel sub-agents) |
| `council` | `council_review` builtin |
| `vision` | vision turns when default provider lacks capability |
| `audio` | audio turns |
| `pdf` | pdf turns |

## Reference

- `internal/compute/providers.go` — `ProviderRegistry` + capability matching
- `internal/modelsdev/` — catalog fetcher + cache
- `internal/compute/capability_modelsdev.go` — auto-capability merger
- `pkg/config/config.go` — schema (`ProviderConfig`, `ComputeConfig`)
