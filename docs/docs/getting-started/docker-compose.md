---
sidebar_position: 2
---

# Docker Compose

The fastest way to run lobslaw — single-node by default, with multi-node as a config tweak. Built for podman 5.x; works with docker-compose with one tweak.

If you just want one node (the common case), use `compose.yml` (or comment out `node-2`/`node-3` in the cluster file). If you want a 3-node fault-tolerant cluster on a single host for testing, use `cluster.yml`.

## 1. Clone

```bash
git clone https://github.com/jmylchreest/lobslaw
cd lobslaw/deploy/docker
```

## 2. Bootstrap secrets

```bash
./bootstrap.sh
```

This generates:
- `secrets/ca.pem` + `secrets/ca-key.pem` — the cluster CA.
- `secrets/node-{1,2,3}.{pem,key}` — per-node mTLS certs signed by that CA.
- `.env` — populated with placeholders for the keys you need to fill in.

## 3. Fill in `.env`

Edit `.env`:

```sh
TELEGRAM_BOT_TOKEN=123456:abcdef-your-bot-token
OPENROUTER_API_KEY=sk-or-v1-...

# Optional, only if you want OAuth providers:
GOOGLE_OAUTH_CLIENT_ID=...
GOOGLE_OAUTH_CLIENT_SECRET=...
GITHUB_OAUTH_CLIENT_ID=...
GITHUB_OAUTH_CLIENT_SECRET=...
```

## 4. Bring it up

```bash
podman compose -f cluster.yml up -d   # or docker compose ...
```

You should see:

```
[+] Running 3/3
 ✔ Container lobslaw-node-1   Started
 ✔ Container lobslaw-node-2   Started
 ✔ Container lobslaw-node-3   Started
```

Logs:

```bash
podman compose -f cluster.yml logs -f node-1
```

Look for:

```
INFO  raft leadership changed is_leader=true
INFO  policy: seeded default builtin rules count=...
INFO  egress: smokescreen proxy started bind=127.0.0.1:NNNNN roles=...
INFO  lobslaw node started node_id=node-XXX
```

## 5. Talk to it

The cluster's gateway is published on host port 8443 by default (only node-1 — followers are reachable via `compose exec` if you want to test gateway behaviour on a non-leader).

Open Telegram, find your bot, send any message. You should get a reply.

If you don't, see [doctor](/operating/doctor) and [Troubleshooting](#troubleshooting) below.

## What's running

For the cluster-mode case (`cluster.yml`):

- **3 nodes** sharing Raft consensus over the cluster CA-signed mTLS mesh on port 7000 (raft) + 8443 (gateway).
- **A shared bolt store** replicated via Raft — every memory write goes through consensus.
- **A smokescreen forward proxy** per node, intercepting all subprocess egress and per-role ACL'd.
- **An agent loop** on each node — only the Raft leader serves user-initiated turns; followers wait.

For single-node, the same minus the consensus chatter — Raft is still the data path, but with a one-member peer set, every entry commits immediately.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Containers exit `code=1` immediately | Run `bootstrap.sh` first; missing certs |
| `dial tcp ... no route to host` between nodes | Firewall blocking 7000/8443 inside the compose network |
| Agent says "Request rejected by proxy" | Egress ACL doesn't allow that host — see [Egress and ACL](/security/egress-and-acl) |
| Bot doesn't respond | `TELEGRAM_BOT_TOKEN` empty or wrong; check `node-1` logs for telegram-getUpdates errors |

## Next

- **Send your first useful message** → [First message](/getting-started/first-message)
- **Install a skill** → [Features → ClawHub](/features/clawhub)
- **Open up the security model** → [Security → Threat model](/security/threat-model)
