---
sidebar_position: 2
---

# Doctor

`lobslaw doctor` runs a fixed checklist against your config + environment. Every check passes or fails with a one-line diagnostic.

## Check categories

```bash
lobslaw doctor --config config.toml
```

Output groups:

```
PASS  config valid
PASS  .env permissions       chmod 0600 ✓
FAIL  LLM provider reachable openrouter.ai:443 dial timeout — check connectivity or smokescreen ACL
PASS  egress role=clawhub    clawhub.ai:443 reachable
WARN  oauth provider=google  no client_id_ref configured (skip if not using google)
PASS  certs valid            ca.pem expires 2027-04-28; node.pem expires 2026-07-30
PASS  raft listen 0.0.0.0:7000
PASS  bolt store openable    data/state.db (3.2 MB)
PASS  audit dir writable     audit/
```

## Specific checks

| Category | What it checks |
|---|---|
| `config` | TOML parses; required fields present; no schema mismatches |
| `permissions` | `.env` is `0600`; cert keys are `0400` |
| `connectivity` | Each provider endpoint, each gateway listen port, each MCP server command exists |
| `egress` | Each smokescreen role connects to a known-good and rejects a known-bad |
| `oauth` | Each declared provider's endpoints resolve |
| `clawhub` | If `clawhub_base_url` set, hit `<base>/api/v1/health` |
| `certs` | CA + node certs parse; not expired; not expiring within 7 days; CN matches node ID |
| `raft` | Listen address binds; raft data dir writable |
| `bolt` | state.db opens; magic bytes match; not corrupted |
| `audit` | Audit dir exists, writable, rotates as expected |

## Filtering

```bash
lobslaw doctor --check connectivity        # only connectivity
lobslaw doctor --check connectivity,certs  # multiple
```

## CI integration

`doctor` exits with status 0 if every check passes, 1 if any fails. Useful in CI:

```yaml
- name: Verify config
  run: lobslaw doctor --config config.toml --log-format json
```

JSON output is machine-readable for downstream parsing.

## What it doesn't check

- Runtime tool execution (no synthetic agent turns).
- Memory store contents (use `cmd/inspect` for that).
- Whether your `SOUL.md` is sensible (subjective).

## Reference

- `cmd/lobslaw/doctor.go` — check definitions
