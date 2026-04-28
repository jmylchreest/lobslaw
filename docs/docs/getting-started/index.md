---
sidebar_position: 1
---

# Getting Started

There are three ways to run lobslaw:

| Path | Audience | Time |
|---|---|---|
| [Docker Compose](/getting-started/docker-compose) | Anyone who wants it running fast — single-node, or a 3-node test cluster | ~5 min |
| [From source](/getting-started/from-source) | Developers, custom builds, non-Linux hosts | ~15 min |
| [First message](/getting-started/first-message) | After it's running — verify it works end-to-end | ~2 min |

**Most people run a single node.** Multi-node is only worth the operational overhead if you actually need fault tolerance — same machine going down, etc. The single-node case is the default; the multi-node case is a config tweak (just add peers to `[cluster] peers`).

## What you need before you begin

Regardless of path:

- **An LLM API key.** OpenRouter is the easiest default ([openrouter.ai](https://openrouter.ai/)). Anthropic, OpenAI, MiniMax, anything that speaks the `/v1/chat/completions` shape.
- **A Telegram bot token** (talk to [@BotFather](https://t.me/BotFather)) — the easiest gateway to test with. REST works too; Slack/Discord don't yet.
- **Optionally**, a Google / GitHub / Microsoft / GitLab OAuth client if you want to install skills that talk to those services.

That's it. Everything else (mTLS certs, raft state, vector index) is generated on first boot.
