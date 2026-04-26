# lobslaw docker stack

Single-node compose deployment with init containers for cert
generation and tools provisioning.

## Layout

```
deploy/docker/
├── docker-compose.yml      stack definition (4 services + 5 volumes)
├── config.toml             host-editable lobslaw config (bind-mounted ro)
├── SOUL.md                 host-editable soul (bind-mounted ro)
├── .env.example            copy to .env, fill in secrets + hostname
├── .gitignore              keeps .env + secrets/*.pem out of git
└── secrets/                CA material — populate before first up
    ├── ca.pem              cluster CA public cert
    └── ca-key.pem          cluster CA private key (never copied to lobslaw)
```

## Filesystem inside the lobslaw container

| Path                | Mode | Source                         | Purpose                                        |
| ------------------- | ---- | ------------------------------ | ---------------------------------------------- |
| `/usr/local/bin`    | ro   | image                          | the lobslaw binary                             |
| `/lobslaw/bin`      | ro   | tools-init → busybox:glibc     | baseline shell utilities (sh, ls, cat, …)      |
| `/lobslaw/usr/bin`  | rw   | persistent volume              | operator/agent extras (drop binaries here)     |
| `/etc/lobslaw/certs`| ro   | cert-init → docker secret CA   | node cert + node key + CA public cert          |
| `/etc/lobslaw/*`    | ro   | host bind mount                | config.toml + SOUL.md                          |
| `/var/lobslaw/data` | rw   | persistent volume              | raft.db + state.db + snapshots                 |
| `/var/lobslaw/audit`| rw   | persistent volume              | audit.jsonl ring                               |
| `/workspace`        | rw   | host bind mount                | `~/.config/lobslaw/workspace` from the host    |

PATH is `/usr/local/bin:/lobslaw/usr/bin:/lobslaw/bin:/usr/bin:/bin`,
so the agent's `shell_command` builtin finds binaries in the
operator-managed rw layer first, then the read-only baseline.

## First-time setup

### 1. Generate the cluster CA (once per cluster, not per node)

```bash
# From the repo root, anywhere with a built lobslaw binary:
mkdir -p deploy/docker/secrets
go run ./cmd/lobslaw cluster ca-init \
  --ca-cert deploy/docker/secrets/ca.pem \
  --ca-key  deploy/docker/secrets/ca-key.pem
```

For a multi-node cluster, copy `ca.pem` + `ca-key.pem` to each
host's `deploy/docker/secrets/` — every node signs against the same
CA. The CA private key never enters the running lobslaw container.

### 2. Configure environment

```bash
cd deploy/docker
cp .env.example .env

# Generate a memory key.
echo "LOBSLAW_MEMORY_KEY=$(head -c 32 /dev/urandom | base64)" >> .env

# Edit .env: set LOBSLAW_HOSTNAME, LOBSLAW_FAST_API_KEY, etc.
$EDITOR .env
```

### 3. Make sure the workspace dir exists

```bash
mkdir -p ~/.config/lobslaw/workspace
```

### 4. Bring it up

```bash
docker compose up -d
```

First boot does:
1. `tools-init` populates `/lobslaw/bin` from busybox:glibc and
   chowns the rw volume for the nonroot lobslaw user.
2. `cert-init` signs `node-cert.pem` for `$LOBSLAW_HOSTNAME` against
   the CA in `secrets/`.
3. `lobslaw` starts. With no seed_nodes set in `config.toml`, it
   solo-bootstraps a fresh single-voter cluster.

Tail the logs:

```bash
docker compose logs -f lobslaw
```

You should see `raft: bootstrapped a new cluster as sole voter`
followed by an election win.

## Adding a tool the agent can use

```bash
# Drop a binary into the rw extension dir. Visible to lobslaw
# immediately — no restart needed.
docker compose exec tools sh
> wget -O /lobslaw/usr/bin/rg https://github.com/.../ripgrep
> chmod +x /lobslaw/usr/bin/rg
> exit
```

Anything in `/lobslaw/usr/bin` is on PATH for `shell_command` calls
and survives `docker compose down/up`.

## Multi-node, multi-host (production layout)

Each node gets its own `.env` with a distinct `LOBSLAW_HOSTNAME`.
All nodes share the same `secrets/ca.pem` + `secrets/ca-key.pem`.

On the first node (the bootstrap seed), `config.toml` has no
`seed_nodes` and `bootstrap = true` (the default) — it forms the
cluster.

On every joiner, `config.toml` lists the seed and disables
solo-bootstrap so a startup-time partition can't split-brain:

```toml
[cluster]
bootstrap = false              # refuse to form a fresh cluster

[discovery]
seed_nodes = ["lobslaw-1:7443"]
```

Bring up the seed first, then the joiners. The joiner calls
`AddMember` against the seed; the seed (now leader) calls
`AddVoter`; the new node sees itself in the replicated config and
starts following.

## 3-node cluster on a single host (local testing)

`cluster.yml` defines three nodes wired together on a shared compose
network — useful for exercising the bootstrap/join flow + raft
replication without standing up three machines.

```bash
cd deploy/docker
# 1. CA generation (same as single-node)
go run ../../cmd/lobslaw cluster ca-init \
  --ca-cert secrets/ca.pem --ca-key secrets/ca-key.pem

# 2. .env (memory key + LLM key shared by all three nodes)
cp .env.example .env
echo "LOBSLAW_MEMORY_KEY=$(head -c 32 /dev/urandom | base64)" >> .env
$EDITOR .env       # set LOBSLAW_FAST_API_KEY

mkdir -p ~/.config/lobslaw/workspace

# 3. Bring it up.
podman compose -f cluster.yml up -d

# 4. Watch the bootstrap/join handshake.
podman compose -f cluster.yml logs -f lobslaw-1 lobslaw-2 lobslaw-3
```

Expected sequence:

- `lobslaw-1` (no seeds, `bootstrap=true`):
  ```
  raft: bootstrapped a new cluster as sole voter   reason="no seeds configured"
  election won                                       term=2
  singleton acquired                                 name=telegram-poll
  ```
- `lobslaw-2` and `lobslaw-3` (seed=`lobslaw-1:7443`, `bootstrap=false`):
  ```
  cluster join accepted                              via=lobslaw-1:7443
  raft: joined existing cluster via seeds
  ```
- Back on `lobslaw-1`:
  ```
  cluster member added                               peer_id=lobslaw-2
  raft peer changed                                  peer_id=lobslaw-2 suffrage=Voter
  cluster member added                               peer_id=lobslaw-3
  raft peer changed                                  peer_id=lobslaw-3 suffrage=Voter
  ```

After ~5–10 seconds the three nodes form a quorum-of-3 cluster.
Verify by tailing any node's debug snapshot (every 30s):

```bash
podman compose -f cluster.yml exec lobslaw-1 \
  /usr/local/bin/lobslaw --log-level debug   # for one cycle
```

…or just look for the periodic `raft cluster snapshot` lines in the
existing logs. `servers=` should list all three.

### Watching the logs

```bash
# All three nodes, follow live (Ctrl-C to detach):
podman compose -f cluster.yml logs -f lobslaw-1 lobslaw-2 lobslaw-3

# One node only:
podman logs -f docker_lobslaw-1_1

# Last 50 lines from each, no follow:
for n in 1 2 3; do
  echo "=== lobslaw-$n ==="
  podman logs --tail=50 docker_lobslaw-${n}_1
done

# Just the cluster-membership decisions (one line per node):
for n in 1 2 3; do
  podman logs docker_lobslaw-${n}_1 2>&1 \
    | grep -E '"msg":"raft: (joined|bootstrapped|resuming)' | tail -1 | jq -c .
done

# Real-time leadership / peer-change events across the cluster:
podman compose -f cluster.yml logs -f --tail=0 lobslaw-1 lobslaw-2 lobslaw-3 \
  2>&1 | grep --line-buffered -E 'raft leadership|raft peer changed|cluster member added|singleton (acquired|released)'

# Errors only (across the stack):
podman compose -f cluster.yml logs --since 10m lobslaw-1 lobslaw-2 lobslaw-3 \
  2>&1 | jq -c 'select(.level == "ERROR")'

# Periodic raft cluster snapshot (every 30s, requires --log-level debug;
# set LOBSLAW_LOG_LEVEL=debug in .env or override per-service environment):
podman logs docker_lobslaw-1_1 2>&1 \
  | grep '"raft cluster snapshot"' | tail -1 | jq -r .servers
```

Logs are JSON by default in containers (auto-detected because stdout
isn't a TTY). Pipe through `jq` for structured filtering. To see them
as plain text instead, set `LOBSLAW_LOG_FORMAT=text` in the
service's `environment:` block.

### Is replication caught up?

A new joiner finishes catching up when its `applied_index` (entries
the FSM has processed) reaches the leader's `commit_index` (entries
a majority has acknowledged). Three ways to check, easiest first:

**(a) Read raft's own log lines.** `pipelining replication` from the
leader to a peer means the peer is caught up enough that the leader
has switched from the rejection-resync mode to streaming mode. Seen
once per joiner shortly after `cluster member added`:

```bash
podman logs docker_lobslaw-1_1 2>&1 | jq -r 'select(.msg=="pipelining replication") | "  \(.peer.ID) caught up"'
```

**(b) Compare commit/applied indexes via the periodic snapshot.**
Requires `--log-level debug`; the 30s `raft cluster snapshot` line
includes both. Restart with debug logging if needed (see below),
then:

```bash
for n in 1 2 3; do
  echo "=== lobslaw-$n ==="
  podman logs --tail=200 docker_lobslaw-${n}_1 2>&1 \
    | grep '"raft cluster snapshot"' | tail -1 \
    | jq '{state, term, commit_index, applied_index, num_peers, last_contact}'
done
```

`applied_index == commit_index` across all nodes → fully caught up.
A small momentary lag (1-2) is normal even in steady state. A
growing lag means a slow follower or a network problem.

**(c) `debug_raft` agent builtin.** When you have an LLM provider
configured, ask the agent: "use debug_raft and tell me the
applied_index on each node." The builtin returns a structured map
including `caught_up` (bool), `applied_lag` (commit minus applied),
plus the full server list — handy for "summarise cluster health"
prompts.

### Switching to debug logs

The 30-second raft cluster snapshot, hclog passthrough from
hashicorp/raft, and the discovery client's per-packet receive lines
are all DEBUG. To see them on a running stack:

```bash
podman compose -f cluster.yml stop lobslaw-1 lobslaw-2 lobslaw-3
LOBSLAW_LOG_LEVEL=debug podman compose -f cluster.yml up -d \
  --no-deps lobslaw-1 lobslaw-2 lobslaw-3
```

(`--log-level` is hoisted to `LOBSLAW_LOG_LEVEL` by main, so the env
var works for both bare-binary and container invocations.)

### Notes for the 3-node local stack

- Only `lobslaw-1`'s gateway is published on the host (port 8443).
  Followers' gateways are still reachable via
  `podman compose -f cluster.yml exec lobslaw-2 ...` if you want to
  test follower-side gateway behaviour.
- Each node has its own data/audit/certs volume
  (`data-1`/`data-2`/…) so they truly persist independently.
- Workspace is shared across all three (single bind mount from
  `~/.config/lobslaw/workspace`) — that's deliberate for local
  testing. If you want per-node workspaces, give each `lobslaw-N`
  service its own bind path.
- Configuration overrides go through env vars: each node's
  `LOBSLAW__CLUSTER__BOOTSTRAP` and
  `LOBSLAW__DISCOVERY__SEED_NODES__0` differ, but they all share the
  same `config.toml` on disk. This is the koanf double-underscore
  override syntax — `LOBSLAW__SECTION__KEY=value` overrides
  `[section] key = value`; arrays use `__N` (zero-indexed) suffixes.
- Tear down + clean wipe:
  ```bash
  podman compose -f cluster.yml down --volumes
  ```

## Cluster reset

To wipe a node's raft state (e.g. after a hostname change leaves
this node orphaned in the persisted config):

```bash
docker compose down
docker volume rm lobslaw_data lobslaw_audit
docker compose up -d
```

Or, less destructive, exec the lobslaw binary against the volume
to wipe only raft (preserve audit):

```bash
docker compose run --rm cert-init \
  /usr/local/bin/lobslaw cluster reset \
  --data-dir=/var/lobslaw/data --yes
```

(Add a `data:/var/lobslaw/data` volume to cert-init's mounts first
if you want to use the second form regularly.)

## Updating the busybox baseline

```bash
docker compose pull tools-init
docker compose up -d --force-recreate tools-init
# tools-init re-runs and refreshes /lobslaw/bin.
```

## What's intentionally NOT in this stack

- **No npm / python / git / bun** in the lobslaw container. Those
  live in `Dockerfile.tools` (~120MB image). If you want them
  available to `shell_command`, either:
  1. Build `lobslaw-tools:dev` as the main image (swap the
     `lobslaw` service's `dockerfile:` to `Dockerfile.tools`), or
  2. Drop them into `/lobslaw/usr/bin` at runtime via the tools
     sidecar.
- **No raft over the public internet.** The mTLS cluster port
  (7443) is published on the host, but a real multi-node deployment
  should keep that port firewalled to the cluster's private
  network. The CA-signed cert is the only auth — anyone who can
  reach the port and present a CA-signed cert is a peer.
- **No backup of the data volume.** Raft snapshots go to
  `/var/lobslaw/data/snapshots` (declared as `local-snapshots` in
  `config.toml`). For off-host durability, add a `[[storage.mounts]]`
  with type `s3` / `r2` and point `[memory.snapshot] target` at it.
