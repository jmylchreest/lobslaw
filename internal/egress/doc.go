// Package egress is the single chokepoint for outbound HTTPS from
// lobslaw. Every http.Client constructed in this codebase MUST come
// from a factory in this package — a forbidigo lint rule in
// .golangci.yml blocks raw http.Client construction elsewhere.
//
// The factory returns a client preconfigured to route through an
// internal forward proxy (smokescreen, embedded in-process). The
// proxy enforces a per-role ACL of allowed destination hostnames,
// generated from declared lobslaw config (compute.providers,
// gateway.channels, mcp.servers, skill manifests, clawhub config).
//
// Roles identify the call site:
//
//	"llm"                       — LLM provider HTTPS calls
//	"embedding"                 — embedding provider
//	"fetch_url"                 — the agent's fetch_url builtin
//	"gateway/telegram"          — Telegram channel poller / sender
//	"gateway/webhook/<channel>" — per-channel webhook upstream
//	"mcp/<name>"                — MCP server upstream
//	"skill/<name>"              — skill subprocess HTTPS_PROXY target
//	"clawhub"                   — clawhub.ai installer
//	"oauth/<provider>"          — provider token endpoints (Google, etc.)
//	"credentials/<provider>"    — refresh-token rotation calls
//
// The Provider is set once at node boot via SetActiveProvider. Tests
// install a noop provider; production installs the smokescreen-backed
// provider. Swapping the implementation (Envoy, eBPF, no-op for
// constrained deployments) is a one-Provider-implementation change
// without touching call sites.
package egress
