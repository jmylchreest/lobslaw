---
sidebar_position: 5
---

# Egress and ACL

Every byte of subprocess egress traffic flows through a **smokescreen forward proxy** with a per-role host allowlist.

## Why a forward proxy?

Three properties that other approaches don't give you:

1. **Layer-7 visibility.** The proxy sees the actual hostname being requested (CONNECT for HTTPS, Host header for HTTP), so an ACL can be `["api.github.com", "objects.githubusercontent.com"]` rather than IP CIDR ranges that GitHub rotates.
2. **Per-role separation.** A skill subprocess is told to use `HTTPS_PROXY=http://skill%2Fgws-workspace:_@127.0.0.1:NNNNN`. The role identifier is encoded as the basic-auth username; smokescreen extracts it and applies the matching ACL. Same proxy, same port, different roles.
3. **Default-deny RFC1918.** Smokescreen blocks all private IP ranges by default, so a tool can't pivot to your home network even if it bypasses the proxy's hostname rules via DNS rebinding.

We use [Stripe's smokescreen](https://github.com/stripe/smokescreen) embedded directly into the lobslaw binary — no sidecar process.

## Roles

A **role** is a labelled allowlist. Built-in roles:

| Role | Hosts | Used by |
|---|---|---|
| `llm` | every configured `[[compute.providers]]` endpoint host | agent → LLM |
| `embedding` | embedding endpoint host | embedder |
| `gateway/telegram` | `api.telegram.org` | telegram channel |
| `fetch_url` | configurable; default permissive minus RFC1918 | `fetch_url` builtin |
| `web_search` | search provider hosts | `web_search` builtin |
| `clawhub` | `clawhub_base_url` host + binary hosts | `clawhub_install` builtin |
| `oauth/<provider>` | per-provider device + token endpoints | OAuth flow |
| `skill/<name>` | hosts declared in skill manifest `network` | skill subprocess |
| `mcp/<server>` | hosts declared in `[mcp.servers.<name>]` | MCP server subprocess |

Roles are computed at boot in `internal/egress/builder.go` from the merged config. Operators don't write roles directly — they configure the upstream services and the builder synthesises the ACL.

## Subprocess wiring

When the agent spawns a skill subprocess:

```
HTTPS_PROXY=http://skill%2Fgws-workspace:_@127.0.0.1:7867
HTTP_PROXY =http://skill%2Fgws-workspace:_@127.0.0.1:7867
NO_PROXY   =                                   # cleared
```

The subprocess's HTTP library does Basic auth on the proxy CONNECT preamble. Smokescreen extracts the role (`skill/gws-workspace`), looks up the hosts allowed for it, and either tunnels or rejects. The literal `_` password is non-secret; smokescreen ignores it.

For netns-isolated skills (`network_isolation: true`), TCP loopback is unreachable — the subprocess uses the `[security] egress_uds_path` Unix-domain socket instead. The proxy listens on both transports; UDS connections still carry the role identifier in the Proxy-Authorization header.

## Configuring egress

Most egress configuration is **derived** from other config blocks (provider endpoints, channel definitions, skill manifests). The handful of explicit knobs:

```toml
[security]
egress_upstream_proxy        = "http://corp:8080"     # if behind a corporate proxy
egress_allow_private_ranges  = false                  # NEVER true in production
egress_allow_ranges          = ["100.64.0.0/10"]      # explicit RFC1918 holes (e.g. tailnet)
egress_uds_path              = "/tmp/lobslaw-egress.sock"  # required when any skill uses netns
fetch_url_allow_hosts        = ["api.example.com"]    # narrow fetch_url to specific hosts
clawhub_base_url             = "https://clawhub.ai"
clawhub_binary_hosts         = ["github.com", "objects.githubusercontent.com"]
```

`egress_allow_private_ranges = true` is a footgun and only acceptable in single-machine dev setups where the LLM endpoint is also on loopback.

## ACL hot-reload

The egress builder rebuilds the ACL on every `config.toml` SIGHUP. Live connections aren't disrupted; new tunnels are evaluated against the new ACL. See `internal/egress/smokescreen.go:Reload`.

## Verifying egress

The `lobslaw doctor` command exercises each role with a known-good and known-bad target:

```bash
lobslaw doctor --check egress
```

Outputs:

```
PASS  egress role=llm                           openrouter.ai:443 reachable
PASS  egress role=skill/gws-workspace          oauth2.googleapis.com:443 reachable
PASS  egress role=skill/gws-workspace          attacker.com:443 BLOCKED (expected)
FAIL  egress role=clawhub                      clawhub.ai:443 BLOCKED — set [security] clawhub_base_url
```

## Common pitfalls

- **"Request rejected by proxy"** — the role's allowlist doesn't include that host. Either narrow what the agent is asked to do, or extend the role (typically by configuring the upstream service in the right block).
- **`clawhub_install` fails reaching clawhub.ai** — `[security] clawhub_base_url` not set. Setting it both opens the egress role *and* enables the install pipeline.
- **A skill says "no network"** — the manifest declared `network: []`. Either add the hosts to the manifest, or grant via an operator-side override block.

## Reference

- `internal/egress/builder.go` — role assembly from config
- `internal/egress/smokescreen.go` — embedded forward proxy
- `internal/egress/doc.go` — package overview
- `pkg/config/config.go` — `SecurityConfig` schema
