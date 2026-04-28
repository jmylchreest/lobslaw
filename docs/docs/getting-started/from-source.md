---
sidebar_position: 3
---

# From Source

For developers, custom builds, or non-Linux hosts where the Docker image isn't a fit.

## Prerequisites

- Go 1.22+
- Linux (for full sandbox features). macOS works for everything except Landlock + seccomp + nftables — namespacing falls back to chroot+cwd containment.
- Optional: `nft` binary in `$PATH` if you want nftables egress redirection.

## 1. Build

```bash
go build -o ./bin/lobslaw ./cmd/lobslaw
```

There's also `cmd/inspect` for poking at a stopped node's bolt store, and `cmd/backfill-embeddings` if you've changed the embedding model and need to re-index.

## 2. Initialize

```bash
mkdir -p ~/.config/lobslaw
cd ~/.config/lobslaw
~/path/to/lobslaw init
```

Interactive prompts produce:

- `config.toml` — main config.
- `.env` — secrets, chmod 0600.
- `data/` — raft state, vector index, episodic memory.
- `audit/` — JSONL audit log.
- `certs/` — generated next.

## 3. mTLS bootstrap

```bash
./bin/lobslaw cluster ca-init \
  --ca-cert certs/ca.pem --ca-key certs/ca-key.pem

./bin/lobslaw cluster sign-node \
  --ca-cert certs/ca.pem --ca-key certs/ca-key.pem \
  --node-cert certs/node.pem --node-key certs/node-key.pem \
  --node-id $(./bin/lobslaw nodeid)
```

For single-node — the common case — the CA private key never needs to leave this host. For multi-node, copy `ca.pem` (only the cert; not the key) to peer hosts and run `cluster sign-node` on each.

## 4. Verify

```bash
./bin/lobslaw doctor --config config.toml
```

Every check should pass. Common surprises:

- `.env` not 0600 → `chmod 0600 .env`
- LLM provider unreachable → wrong endpoint, or smokescreen ACL too tight
- `clawhub_base_url` set without operator policy rule → harmless, but the agent can't actually install

## 5. Boot

```bash
./bin/lobslaw --config config.toml
```

You're up. Send your bot a message.

## 6. SIGHUP for cert rotation

When you replace `certs/node.pem` + `certs/node-key.pem`:

```bash
kill -HUP $(pidof lobslaw)
```

Atomic-swap. In-flight TLS connections aren't disrupted; new handshakes pick up the rotated material. See [Cert rotation](/operating/cert-rotation).
