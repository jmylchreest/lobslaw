---
sidebar_position: 2
---

# Containers

Two supported ways to run lobslaw in a container. Both live in `deploy/`.

| | `deploy/podman/` | `deploy/docker/` |
|---|---|---|
| Shape | one container, one host directory | compose stack: cert-init + node + shell sidecar |
| State | bind mounts under a single directory | named volumes |
| Good for | a local node you want to read, edit and wipe by hand | the deployment shape that scales to multiple hosts |
| Cluster | single node only | `cluster.yml` runs three nodes on one host |

Everything below assumes you have cloned the repo:

```bash
git clone https://github.com/jmylchreest/lobslaw
cd lobslaw
```

## Option A — one container (podman)

Everything the node persists lives in one directory, so you can read its
config in an editor and wipe it with `rm -rf`.

```bash
cd deploy/podman
./lobslaw-local build
./lobslaw-local up
```

`up` bootstraps on first run — cluster CA, node certificate,
`config.toml`, `SOUL.md`, a memory encryption key, and the builtin
embedding model — and does nothing on runs after that. State goes to
`~/.local/share/lobslaw-local` by default; `./lobslaw-local path` prints
it.

The node starts without an LLM key. Memory, policy, storage, the
scheduler, audit and the REST gateway all work; only the agent loop
needs a provider:

```bash
$EDITOR "$(./lobslaw-local path)/.env"     # LOBSLAW_MINIMAX_API_KEY=...
./lobslaw-local restart
./lobslaw-local ask "what tools do you have?"
```

See `deploy/podman/README.md` for the full command list.

## Option B — compose (docker or podman)

```bash
cd deploy/docker
```

### 1. Generate the cluster CA

Once per cluster, not per node. The CA private key is mounted only into
the one-shot `cert-init` container and never into the running node.

```bash
mkdir -p secrets
go run ../../cmd/lobslaw cluster ca-init \
  --ca-cert secrets/ca.pem \
  --ca-key  secrets/ca-key.pem
```

### 2. Write `.env`

```bash
cat > .env <<EOF
LOBSLAW_HOSTNAME=lobslaw-1
LOBSLAW_MEMORY_KEY=$(head -c 32 /dev/urandom | base64)
LOBSLAW_MINIMAX_API_KEY=
LOBSLAW_OPENROUTER_API_KEY=
LOBSLAW_TELEGRAM_BOT_TOKEN=
LOBSLAW_EXA_API_KEY=
EOF
$EDITOR .env
```

`LOBSLAW_MEMORY_KEY` encrypts the memory FSM. Losing it makes the corpus
unreadable, so keep a copy somewhere you trust.

Provider key variables follow `LOBSLAW_<LABEL>_API_KEY`, where `<LABEL>`
is a `[[compute.providers]].label` in `config.toml`. Rename them if you
rename a provider.

### 3. Create the workspace directory

Bind-mounted into the node as `/workspace`.

```bash
mkdir -p ~/.config/lobslaw/workspace
```

### 4. Bring it up

```bash
docker compose up -d          # or: podman compose up -d
docker compose logs -f lobslaw
```

`cert-init` signs this node's certificate, then `lobslaw` starts and —
with no seeds configured — bootstraps a single-voter cluster:

```
INFO raft: bootstrapped a new cluster as sole voter node_id=lobslaw-1
INFO raft leadership changed is_leader=true
INFO policy: seeded default builtin rules count=44
INFO egress: smokescreen proxy started bind=127.0.0.1:NNNNN roles=8
INFO rest server listening addr=[::]:8443
```

### 5. Talk to it

The REST gateway is published on host port 8443:

```bash
curl -s localhost:8443/healthz
curl -sX POST localhost:8443/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"message":"hello"}'
```

It is plaintext HTTP with no authentication in front of it, so keep it
on a trusted interface or terminate TLS ahead of it.

To use Telegram instead, uncomment the telegram channel in
`config.toml`, put your numeric user id in
`[gateway.channels.user_scopes]`, and set `LOBSLAW_TELEGRAM_BOT_TOKEN`.
Ids, never usernames — usernames change. Anyone not listed is dropped.

## Three nodes on one host

`cluster.yml` exercises the bootstrap/join flow and raft replication
without three machines. Bring the seed up alone first, or several fresh
nodes will hear each other with no leader among them:

```bash
podman compose -f cluster.yml up -d lobslaw-1
# wait ~5s for it to elect itself
podman compose -f cluster.yml up -d lobslaw-2 lobslaw-3 tools
podman compose -f cluster.yml logs -f lobslaw-1 lobslaw-2 lobslaw-3
```

`deploy/docker/README.md` has the full walkthrough, including how to
tell whether a follower has caught up.

## What's running

- **The node**, holding raft state in a bolt store. Every memory write
  goes through consensus; with one member, entries commit immediately.
- **A smokescreen forward proxy** per node, intercepting subprocess
  egress and applying a per-role allowlist.
- **The agent loop**, on the raft leader only. Followers wait.

Ports: 7443 for the mTLS cluster mesh, 8443 for the gateway.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Container prints CLI usage and exits 0 | Image predates the default `CMD ["run"]`; rebuild, or set `command: run` |
| Exits immediately with a cert error | `cert-init` didn't run or `secrets/` is empty — generate the CA |
| `bind: address already in use` | Another node already has 7443/8443; change `LOBSLAW_CLUSTER_PORT` / `LOBSLAW_GATEWAY_PORT` |
| `dial tcp ... no route to host` between nodes | Firewall blocking 7443 inside the compose network |
| "Request rejected by proxy" | The egress allowlist doesn't cover that host — see [Egress and ACL](/security/egress-and-acl) |
| Boot fails fetching an embedding model | The mirror redirects to a host the allowlist doesn't cover; place the checkpoint under `<data_dir>/models/<model>` and leave `download_url` empty |
| Bot doesn't respond | Token empty or wrong, or your id isn't in `user_scopes` — check the logs for `telegram: unknown user` |

Run `lobslaw doctor --config /etc/lobslaw/config.toml` inside the
container for a checklist of what's wired and what isn't.

## Next

- **Send your first useful message** → [First message](/getting-started/first-message)
- **Install a skill** → [Features → ClawHub](/features/clawhub)
- **Open up the security model** → [Security → Threat model](/security/threat-model)
