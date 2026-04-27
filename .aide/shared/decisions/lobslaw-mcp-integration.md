---
topic: lobslaw-mcp-integration
decision: "Top-level [mcp.servers] config in TOML spawns MCP subprocesses at boot. Mirrors the existing plugin-manifest loader pattern but operator-direct — no plugin wrapping needed for personal integrations (Gmail, Slack, GitHub). Loader.StartDirect is the new entry point alongside Start (plugin-driven). Secret refs (env:/file:/kms:) resolve the same way every other lobslaw secret does. Each node runs its own child processes (stdio transport can't be shared across nodes); config is pulled from [mcp] at boot — Raft replication of MCP config is deferred (runtime management tools + bucket sync come in a follow-up)."
date: 2026-04-24
---

# lobslaw-mcp-integration

**Decision:** Top-level [mcp.servers] config in TOML spawns MCP subprocesses at boot. Mirrors the existing plugin-manifest loader pattern but operator-direct — no plugin wrapping needed for personal integrations (Gmail, Slack, GitHub). Loader.StartDirect is the new entry point alongside Start (plugin-driven). Secret refs (env:/file:/kms:) resolve the same way every other lobslaw secret does. Each node runs its own child processes (stdio transport can't be shared across nodes); config is pulled from [mcp] at boot — Raft replication of MCP config is deferred (runtime management tools + bucket sync come in a follow-up).

## Rationale

internal/mcp/ client library existed but nothing consumed it from the top-level config. Plugin-driven path works for shared skill bundles; direct [mcp.servers] is for personal-assistant integrations the operator manages themselves. Keeps integration shape simple: one TOML block, secrets as refs, plugin manifests continue to work alongside.

