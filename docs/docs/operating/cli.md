---
sidebar_position: 1
---

# CLI

The `lobslaw` binary is multi-mode — the same binary handles run, init, doctor, plugin install, cert signing, sandbox-exec helper.

## Subcommand list

```
lobslaw                    # run the node (with --config)
lobslaw init               # interactive config scaffold
lobslaw doctor             # config + connectivity checks
lobslaw nodeid             # derive a deterministic node ID for this host
lobslaw cluster            # cluster + cert lifecycle
  cluster ca-init          # create cluster CA
  cluster sign-node        # sign a node cert against the CA
  cluster reset            # nuke the local raft state (DESTRUCTIVE)
lobslaw plugin             # plugin lifecycle
  plugin install <bundle>  # install a clawhub bundle
  plugin list              # list installed skills
lobslaw audit              # query audit logs
lobslaw sandbox-exec       # hidden — used by the sandbox reexec helper
lobslaw dispatch           # hidden — used by hooks / scheduler dispatch
```

## Global flags

```
--config <path>            # config.toml path (required for run, doctor)
--log-level <debug|info|warn|error>
--log-format <text|json>
```

## `lobslaw` (no subcommand)

Runs the node:

```bash
lobslaw --config /etc/lobslaw/config.toml
```

Foreground process. SIGTERM for graceful shutdown, SIGHUP for config reload + cert reload.

## `lobslaw init`

Interactive scaffold — walks through prompts, writes `config.toml`, `.env`, `data/`, `audit/`, `certs/`. See [Getting Started → From Source](/getting-started/from-source) for the full walkthrough.

## `lobslaw doctor`

Runs every health check. See [Doctor](/operating/doctor) for the check list.

```bash
lobslaw doctor --config config.toml
```

## `lobslaw cluster ca-init`

```bash
lobslaw cluster ca-init \
  --ca-cert certs/ca.pem \
  --ca-key  certs/ca-key.pem
```

Generates a fresh ed25519 CA. One-time, per cluster.

## `lobslaw cluster sign-node`

```bash
lobslaw cluster sign-node \
  --ca-cert  certs/ca.pem \
  --ca-key   certs/ca-key.pem \
  --node-cert certs/node.pem \
  --node-key  certs/node-key.pem \
  --node-id  $(lobslaw nodeid)
```

Signs a node keypair against the CA. CN = node ID, SAN = `<id>` + `<id>.cluster.local`.

## `lobslaw plugin install <bundle>`

```bash
# from clawhub
lobslaw plugin install clawhub:gws-workspace@1.0.0

# from local directory
lobslaw plugin install file:///path/to/manifest-dir/

# from a git repo (planned)
lobslaw plugin install git://github.com/owner/skill@v1.0.0
```

Honours `[security] clawhub_signing_policy`.

## `lobslaw audit`

```bash
lobslaw audit --since "1 hour ago" --filter "decision=deny"
```

Pretty-prints the daily JSONL audit log. Filters: `--decision`, `--subject`, `--action`, `--resource`, `--since`.

## `lobslaw sandbox-exec`

Hidden subcommand — invoked only by the sandbox reexec helper, never by the operator directly. Reads `LOBSLAW_SANDBOX_POLICY` env, installs NoNewPrivs + Landlock + seccomp, then `execve`s the target. See [Sandbox](/security/sandbox).
