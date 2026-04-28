---
sidebar_position: 3
---

# Cert Rotation

How to rotate mTLS material without restarting the node.

## Node certs (the common case)

When a node cert is approaching expiry (lobslaw warns at 7 days):

```bash
# 1. Sign a new cert against the same CA
lobslaw cluster sign-node \
  --ca-cert  certs/ca.pem \
  --ca-key   certs/ca-key.pem \
  --node-cert certs/node.pem.new \
  --node-key  certs/node-key.pem.new \
  --node-id  $(lobslaw nodeid)

# 2. Atomic swap (write the new files into place)
mv certs/node.pem.new certs/node.pem
mv certs/node-key.pem.new certs/node-key.pem

# 3. Tell the running node
kill -HUP $(pidof lobslaw)
```

The SIGHUP handler calls `mtls.NodeCreds.Reload()`, which atomically swaps the cert behind an `atomic.Pointer`. In-flight TLS handshakes finish on the old cert; the next handshake picks up the new one.

You should see in the logs:

```
INFO  mtls: certs reloaded subject=CN=node-XXX serial=NEW
```

## CA cert

Rotating the CA is a bigger operation — every node needs to trust the new CA before peers can present new certs:

1. Generate the new CA (`cluster ca-init` to a new path).
2. Update each node's config to trust **both** old and new CAs (a `[cluster.mtls] ca_certs = [...]` list — accepting either signs).
3. SIGHUP every node — they now trust both.
4. Sign new node certs against the new CA, deploy + SIGHUP each.
5. Once every node is on a new-CA-signed cert, remove the old CA from each node's trust list.
6. SIGHUP each. Old CA no longer trusted.

This ratchet pattern avoids a window where any node has a cert nothing else trusts.

## CA private key handling

Single-node deployments: the CA key lives wherever you generated it. Keep it readable only by your user (`chmod 0400`).

Multi-node deployments: keep the CA key on a designated bootstrap host. Sign new node certs there; copy them to the target node over a side channel (SSH). Don't replicate the CA key to nodes that don't need to sign.

The roadmap includes a "sign over the cluster" flow where any node can request a fresh cert and the leader signs it (with the CA key still scoped to the leader). Not wired today.

## Audit

Cert reloads are audit-logged:

```json
{"ts":"2026-04-28T13:45:00Z","event":"mtls_reload","subject":"CN=node-1","serial_old":"01","serial_new":"02"}
```

Useful for verifying rotations completed across a fleet.

## Reference

- `pkg/mtls/mtls.go` — `NodeCreds.Reload`
- `cmd/lobslaw/main.go` — SIGHUP handler
- `cmd/lobslaw/cluster.go` — `cluster sign-node` subcommand
