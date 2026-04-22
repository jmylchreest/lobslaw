# Discovery

How lobslaw nodes find each other, and which mechanism to use for your deployment topology.

## TL;DR

Every node needs to reach at least one other node to join the cluster. Three ways, in descending order of "does this work anywhere":

1. **Seed list** (works everywhere)
2. **DNS SRV / A record expansion** in seed list (works where DNS works)
3. **UDP broadcast** (same-L2-network only)

Pick based on your topology — see the matrix below.

---

## Seed list

Static list of `host:port` entries in `config.toml`. Every node tries each in turn on startup; a single reachable peer is enough to bootstrap.

```toml
[discovery]
seed_nodes = ["node-2.home.arpa:7443", "node-3.home.arpa:7443"]
```

Works over any network where the listed host resolves and the gRPC port is reachable — same LAN, Tailscale, WireGuard, k8s cluster IP, whatever.

## Seed list with DNS expansion

Two special prefixes trigger DNS lookup at startup and expand to multiple concrete addresses:

### `srv:<srv-name>` — SRV record expansion

```toml
[discovery]
seed_nodes = ["srv:_cluster._tcp.lobslaw.default.svc.cluster.local"]
```

Queries SRV records for the given name. Each answer becomes a `host:port` seed. Ideal for k8s where a headless Service auto-creates SRV records for every pod.

### `dns:<host>:<port>` — A record expansion

```toml
[discovery]
seed_nodes = ["dns:lobslaw.home.arpa:7443"]
```

Queries A/AAAA records for the given host. Each answer combined with the fixed port becomes a seed. Useful when your DNS has round-robin A records for cluster members.

Entries mix freely:

```toml
[discovery]
seed_nodes = [
  "srv:_cluster._tcp.lobslaw.default.svc.cluster.local",
  "dns:lobslaw.home.arpa:7443",
  "node-legacy.example.com:7443",
]
```

Unresolvable entries are logged at warn level and skipped — a single reachable address is enough to join.

## UDP broadcast

```toml
[discovery]
broadcast          = true
broadcast_port     = 7445   # default; must match across the LAN
broadcast_interval = "30s"  # default
```

Each node emits a tiny UDP announce packet (`{node_id, address}`) periodically. Listening nodes fold the sender into their peer registry, which triggers a gRPC dial.

**The packet is a hint, not a trust boundary.** The actual cluster-join handshake still requires mTLS with the cluster CA, so a spoofed packet can at worst waste a dial attempt.

## Deployment matrix

| Deployment | Recommended mechanism | Why |
|---|---|---|
| **Single host (dev)** | `broadcast = true` OR seed list with `127.0.0.1:7443` | Either works; broadcast is zero-config. |
| **Home LAN** (laptop + NAS + Pi) | `broadcast = true` | Same-L2 network; UDP broadcast just works. |
| **Docker Compose** | Seed list with service names | Compose gives every service an A record. `seed_nodes = ["lobslaw-2:7443"]`. |
| **Podman** (rootful or rootless with aardvark-dns) | Seed list with container names | Same as Compose — A records via aardvark. |
| **Kubernetes** | `srv:` seed against a headless Service | `clusterIP: None` + named port → CoreDNS auto-creates SRV records. StatefulSet gives stable pod DNS names for voter IDs. |
| **Tailscale-connected fleet** | Seed list with MagicDNS names | `seed_nodes = ["home-server.tailnet.ts.net:7443"]`. Tailscale's DNS resolves these over the tunnel. UDP broadcast does NOT traverse Tailscale. |
| **WireGuard mesh** | Seed list with assigned peer IPs | WireGuard is L3; no broadcast. Use the `Address = ...` peer IPs from your WG config. |
| **Nebula / OpenVPN (tun)** | Seed list | Same — L3 tunnels, no broadcast. |
| **ZeroTier / OpenVPN (tap) / tinc (switch)** | `broadcast = true` works, OR seed list | L2 overlays that do forward broadcasts. Broadcast is zero-config here too. |
| **Multi-region / across internet** | `srv:` against operator-controlled DNS (Cloudflare, Route53), or static seed list | DNS is the only common denominator. |

## Cross-tunnel reachability quick reference

UDP broadcast is a layer-2 concept. Most tunnels are layer-3.

| Tunnel | UDP broadcast traverses? |
|---|---|
| WireGuard | No |
| Tailscale | No |
| Nebula | No |
| IPsec / OpenVPN (tun) | No |
| OpenVPN (tap) | Yes |
| ZeroTier | Yes |
| tinc (switch mode) | Yes |
| k8s pod network | No (each pod is its own broadcast domain) |
| Docker bridge / Podman | Yes within the same network; no across bridges or hosts |

If your nodes aren't on a broadcast-capable network, use seed list (plain or DNS-expanded).

## Membership lifecycle

Once a node has joined the cluster via any of the mechanisms above:

- **Register**: the node announces itself to peers via `NodeService.Register`.
- **Heartbeat**: `NodeService.Heartbeat` every minute (default); stale peers are pruned after 10 minutes of silence.
- **AddMember**: an operator (or another node) can call `NodeService.AddMember` on the current leader to add the joining node as a Raft voter. Call this after the node's registered but isn't yet in the Raft configuration. Followers redirect by returning the current leader's address in the response.
- **Deregister**: on graceful shutdown, `NodeService.Deregister` removes the node from peer registries. Ungraceful shutdown relies on the heartbeat-stale prune path.

The peer registry is per-node in-memory state — it's a superset of the Raft voter configuration (covers compute-only/gateway-only nodes that don't participate in consensus). Raft membership itself is managed via Raft writes.

## Security note

Discovery is a hint layer only:

- UDP broadcast packets and SRV record answers can be forged by anyone on the relevant network/DNS path.
- The actual connection that follows runs mTLS with the cluster CA. A forged discovery hint at worst wastes a TLS handshake attempt.
- So: discovery can be "untrusted" and it's still safe. Only the CA material (per `lobslaw-cluster-bootstrap`) needs real protection.
