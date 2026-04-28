---
sidebar_position: 4
---

# Cluster Membership

Adding and removing nodes (only relevant for multi-node deployments — skip if you're running single-node).

## Adding a node

1. **Generate a node cert** for the new host, signed by the existing CA:

   ```bash
   # On the bootstrap host
   lobslaw cluster sign-node \
     --ca-cert  certs/ca.pem \
     --ca-key   certs/ca-key.pem \
     --node-cert /tmp/node-4.pem \
     --node-key  /tmp/node-4-key.pem \
     --node-id  node-4
   ```

2. **Copy** `ca.pem`, `node-4.pem`, `node-4-key.pem` to the new host.

3. **Configure** the new host's `config.toml` with:

   ```toml
   [cluster]
   node_id = "node-4"
   peers   = ["node-1:7000", "node-2:7000", "node-3:7000"]   # existing
   ```

4. **Boot** lobslaw on the new host.

5. **Add the peer to consensus** — on any existing node:

   ```bash
   # (CLI surface here is roadmap; today this happens via the cluster admin gRPC)
   lobslaw cluster add-peer --id node-4 --address node-4:7000
   ```

The new node fetches a snapshot from the leader, replays the live raft log, joins quorum.

## Removing a node

1. **Stop** the node you want to remove (graceful: `kill -TERM`).

2. **Tell consensus** to drop it:

   ```bash
   lobslaw cluster remove-peer --id node-3
   ```

3. **Wipe** the removed node's data dir if you're decommissioning it (otherwise the bolt files contain everything raft replicated, including encrypted credentials).

## Replacing a failed node

A failed node looks the same as a removed-and-re-added node:

1. `cluster remove-peer --id <failed-id>`
2. Provision a new host (different node ID).
3. Add it via the steps above.

There's no surface for "transplant the failed node's identity" — the new host gets a new ID. Raft considers it a fresh peer.

## Quorum loss

If you lose enough nodes to break quorum (2 out of 3, 3 out of 5, etc.), writes stall. Reads continue.

To recover:

- **If the offline nodes can be brought back:** start them. Quorum reforms, writes resume.
- **If they can't be:** you're in unsafe-recovery territory. `cmd/lobslaw cluster reset` exists for this — but it discards uncommitted entries. Use only if you understand the consequences.

For a personal-assistant cluster, the safer answer is: **run an odd number, on hardware you can recover**, and accept that quorum loss = data-loss-bounded outage.

## Migration

Moving the cluster to new hardware:

1. Stand up new nodes alongside old (so quorum stays intact).
2. Add new nodes to consensus one at a time; each fetches snapshot + log.
3. Remove old nodes one at a time; each step keeps quorum.
4. Decommission old hardware.

Don't try to "back up bolt files and restore on new host" — raft state needs to migrate through proper consensus channels, not file copies.

## Reference

- `internal/discovery/registry.go` — peer registry
- `internal/memory/raft.go` — peer add/remove via `RemovePeer/AddVoter`
- `cmd/lobslaw/cluster.go` — `add-peer`, `remove-peer`, `reset` subcommands
