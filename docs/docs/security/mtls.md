---
sidebar_position: 6
---

# mTLS

All inter-node and gateway traffic is mutual-TLS, signed by a per-cluster CA.

## What's protected

| Surface | Protocol | Auth |
|---|---|---|
| Raft consensus | TCP, length-prefixed framing | mTLS via cluster CA |
| Inter-node gRPC (memory writes, audit replay) | gRPC over HTTP/2 | mTLS via cluster CA |
| Gateway → user | TLS to client (or reverse proxy) | mTLS optional, JWT/OAuth or channel-specific |
| Gateway → cluster | gRPC | mTLS via cluster CA |

Every node has:

- The cluster CA cert (`certs/ca.pem`) — public, distributed.
- Its own keypair signed by the CA (`certs/node.pem` + `certs/node-key.pem`).
- The CA private key (`certs/ca-key.pem`) is **only on the bootstrap host**. Single-node deployments keep it on the host that generated it; multi-node deployments sign on a designated bootstrap node and don't distribute the CA private key.

For a single-node deployment, mTLS is still wired — it covers the gateway listen socket and is the foundation for adding peers later without changing trust roots. You're paying minimal cost for considerable future flexibility.

## Cert rotation

Certs are reloaded by `SIGHUP` without process restart. The mechanism:

```go
type NodeCreds struct {
    cert atomic.Pointer[tls.Certificate]
    paths NodeCredsPaths
}

func (c *NodeCreds) Reload() error {
    new, err := tls.LoadX509KeyPair(c.paths.Cert, c.paths.Key)
    if err != nil { return err }
    c.cert.Store(&new)
    return nil
}
```

`tls.Config.GetCertificate` reads via `c.cert.Load()`. Atomic swap — in-flight handshakes finish with their original cert; the next handshake picks up the new one. No connection interruption.

The `cmd/lobslaw/main.go` SIGHUP handler calls `NodeCreds.Reload` on every signal:

```bash
# After replacing certs/node.pem + certs/node-key.pem:
kill -HUP $(pidof lobslaw)
```

You should see in logs:

```
INFO  mtls: certs reloaded subject=CN=node-XXX serial=...
```

## Bootstrap

```bash
# 1. CA — once per cluster
lobslaw cluster ca-init \
  --ca-cert certs/ca.pem \
  --ca-key  certs/ca-key.pem

# 2. Node cert — once per node
lobslaw cluster sign-node \
  --ca-cert  certs/ca.pem \
  --ca-key   certs/ca-key.pem \
  --node-cert certs/node.pem \
  --node-key  certs/node-key.pem \
  --node-id  $(lobslaw nodeid)
```

`lobslaw nodeid` deterministically derives an ID from the host's `/etc/machine-id` (with operator override via `--node-id-override`). Two nodes can't share an ID; the cert's CN is the ID, so collisions are caught at signing time.

For multi-node:

1. Run `ca-init` on the bootstrap host.
2. Copy `ca.pem` (only — **not** `ca-key.pem`) to peers.
3. On each peer, run `sign-node` against a copy of the CA key kept on the bootstrap host (over a side channel: SSH, password-protected file transfer, etc.).
4. Discard the CA private key when no further joins are anticipated, or keep it offline.

## Trust model

A peer is trusted iff its cert is signed by the cluster CA. There is no per-peer revocation list today (a deferred feature; current alternative is rotating the CA and re-signing every legitimate peer).

The cluster MemoryKey (separate from mTLS) seeds AEAD on the credentials bucket; rotation is the operator's responsibility post-incident.

## Common pitfalls

- **CN must equal node ID.** `cluster sign-node` enforces this; manual signing won't.
- **CA private key on every host.** Don't. It only needs to be where you sign certs, and ideally only when signing.
- **Cert with no SAN.** The signing helper sets a SAN of `<node-id>` and `<node-id>.cluster.local`. Manual workflows that omit SANs cause Go's TLS verifier to reject the handshake.
- **System time skew.** mTLS cert validity is ±5 min by default — large clock drift between peers breaks consensus joins. Run NTP.

## Reference

- `pkg/mtls/mtls.go` — `NodeCreds` + atomic.Pointer reload
- `pkg/mtls/sign.go` — CA + node-cert signing helpers
- `cmd/lobslaw/cluster.go` — `cluster ca-init` + `cluster sign-node` subcommands
- `cmd/lobslaw/main.go` — SIGHUP handler wiring
