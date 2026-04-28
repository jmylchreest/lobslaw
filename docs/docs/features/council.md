---
sidebar_position: 7
---

# Council

Multi-provider LLM review. Send one question to several providers in parallel, get all their answers.

## Use case

> **You:** I've been told we should switch our caching layer from redis to memcached for cost reasons. Get a council on this — independent and adversarial.
>
> **Bot:** Asking 3 providers — anthropic-direct, openrouter, minimax.
>
> *Round 1 (independent):*
>
> *anthropic-direct:* "Memcached is cheaper per-MB for pure cache use cases, but you'll lose redis features ..."
>
> *openrouter:* "Probably yes, but it depends on access patterns. Specifically ..."
>
> *minimax:* "Cost difference is overstated; the migration risk is the bigger factor ..."
>
> *Round 2 (adversarial — each sees others' answers):*
>
> *anthropic-direct (refining):* "Reading the others — minimax has a point on migration risk; revising my answer to ..."
>
> ...
>
> Where they agree: cost difference is real but small. Where they diverge: anthropic and openrouter say the migration is straightforward; minimax flags ops risk that's worth investigating.

## Modes

```
council_review(
  question:  "...",
  providers: ["anthropic-direct", "openrouter", "minimax"],   # optional
  mode:      "independent",        # independent | adversarial
)
```

| Mode | Semantics |
|---|---|
| `independent` (default) | Each provider answers in isolation. Returns N answers. |
| `adversarial` | Round 1 = independent. Round 2 = each provider sees the others' round-1 answers and critiques/refines. Returns N round-1 + N round-2 answers. |

`adversarial` is the more interesting one — providers will often catch each other's mistakes or call out weak reasoning.

## Provider selection

```toml
[compute.roles]
council = ["anthropic-direct", "openrouter", "minimax"]
```

The agent calls `council_review` and the role list is the default fan-out. Override per-call:

```
council_review(question="...", providers=["openrouter", "minimax"])
```

For adversarial reviews you want providers from different families:

| Approach | Why |
|---|---|
| Same family (gpt-4 + gpt-4-turbo) | Bad — they'll converge. Use trivial speed-tier comparison only. |
| Different vendors (anthropic + openai + google) | Good — different RLHF, different training data |
| Mixed open + closed (claude + llama via openrouter) | Excellent for sanity-checking |

## Trust tier interaction

Providers declared `trust_tier = "adversarial"` are reserved for council use — they don't fall back to the primary on rate-limit / failure. The point of an adversarial review is the *different* opinion, so falling back to the primary defeats the purpose.

```toml
[[compute.providers]]
label      = "minimax-adversarial"
endpoint   = "..."
model      = "MiniMax-M2"
trust_tier = "adversarial"
backup     = ""        # explicit none
```

## When to use it

Council is expensive (3-5× normal turn cost). Use when:

- The cost of being wrong is high (architectural decision, security-critical change).
- You suspect the primary's answer might be confidently wrong (prompt-injection-prone, training-data cutoff).
- You want to break out of an echo chamber.

Don't use for: factual lookups, code formatting, simple Q&A.

## Listing providers

```
list_providers()
```

Returns label, trust_tier, capabilities, backup. No model names, no endpoints — these are operator-only. The agent uses this to choose providers for a council.

## Reference

- `internal/compute/builtin_council.go` — `list_providers`, `council_review`
- `internal/compute/providers.go` — provider registry + capability matching
