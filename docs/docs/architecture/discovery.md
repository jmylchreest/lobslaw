---
sidebar_position: 5
---

# Discovery

How nodes find each other to form a Raft cluster.

## Modes

```toml
[discovery]
mode = "static"                    # static | dns | broadcast
```

| Mode | When to use |
|---|---|
| `static` | Production; peers list in `[cluster] peers = [...]` |
| `dns` | Cloud + service-discovery (Consul SRV, k8s headless service) |
| `broadcast` | LAN dev cluster; UDP probe + reply |

## Static

```toml
[cluster]
peers = ["node-2:7000", "node-3:7000"]   # excluding self
```

Simple, fixed, predictable. Recommended for any non-dev deployment.

## DNS

```toml
[discovery]
mode     = "dns"
dns_name = "_raft._tcp.lobslaw.local"
```

Resolves SRV records every `[discovery] dns_refresh_interval` (default 1m). Adds new peers, prunes peers that have disappeared from DNS. Plays well with k8s headless services and Consul SRV catalogues.

## Broadcast

```toml
[discovery]
mode             = "broadcast"
broadcast_listen = "0.0.0.0:7099"
broadcast_target = "255.255.255.255:7099"
```

Each node UDP-broadcasts a small "hello, I'm node-X at IP:port" packet every `broadcast_interval`. Other nodes on the LAN receive the packet, validate cluster ID + CA fingerprint, attempt to add the peer to raft.

For dev only: production should use static or DNS. Broadcast is opportunistic; if a node is silent at the wrong moment the cluster can hand-roll a partition.

## Cluster ID and CA fingerprint

Every peer-discovery packet includes:

- Cluster ID — operator-assigned in `[cluster] cluster_id`.
- SHA-256 fingerprint of the cluster CA cert.

Two clusters running on the same LAN with different IDs/fingerprints don't accidentally merge.

## Adding a node mid-life

Generate a node cert against the cluster CA, drop the binary + config on the new host, start it. Other peers either:

- Already have it in their `peers` list — joins on first attempt.
- Or pick it up via DNS / broadcast.

`hashicorp/raft` handles the actual peer add via `RemovePeer/AddPeer` log entries; the join is consensus-driven, so a misconfigured new node doesn't poison existing peers.

## Reference

- `internal/discovery/registry.go` — peer registry
- `internal/discovery/broadcast.go` — UDP listener + sender
- `internal/discovery/client.go` — DNS resolver
