---
sidebar_position: 2
---

# Cluster

Raft consensus, FSM dispatch, leader semantics.

## Raft library

[hashicorp/raft](https://github.com/hashicorp/raft) for the consensus engine, with bbolt as both the log store and stable store. Snapshot transport uses raft's stream snapshot protocol over the cluster mTLS connection.

## FSM shape

Every mutation is a `LogEntry` proto:

```protobuf
message LogEntry {
  oneof payload {
    EpisodicRecord    episodic           = 1;
    SoulFragment      soul_fragment      = 2;
    ScheduledTask     scheduled_task     = 3;
    AgentCommitment   commitment         = 4;
    PolicyRule        policy_rule        = 5;
    CredentialRecord  credential         = 6;
    CredentialACL     credential_acl     = 7;
    UserPreferences   user_prefs         = 21;
    // ...
  }
}
```

The FSM `Apply(log)` switches on payload type and routes to the right service:

```go
func (f *FSM) Apply(log *raft.Log) any {
    var entry lobslawv1.LogEntry
    proto.Unmarshal(log.Data, &entry)
    switch p := entry.Payload.(type) {
    case *lobslawv1.LogEntry_Episodic:
        return f.episodic.applyLocked(p.Episodic)
    case *lobslawv1.LogEntry_UserPrefs:
        return f.userPrefs.applyLocked(p.UserPrefs)
    // ...
    }
}
```

Each service has an `applyLocked` that writes to its bucket and is called only by the FSM under the FSM mutex. Reads bypass the FSM and go straight to bolt (fast path).

## Leader-only mutations

Only the Raft leader may write, but a user's message arrives at whichever gateway node they reached, and that is uncorrelated with leadership. So a write issued on a follower is **forwarded to the leader** rather than refused.

Writes go through `RaftNode.ApplyOrForward`. On the leader that is a plain local apply; on a follower it sends the marshalled `LogEntry` to the leader's `NodeService.Propose` over the cluster's existing mTLS connection. The Raft transport shares that same gRPC server, so the address Raft reports for the leader is dialable as-is.

Reads are always local and never forwarded — followers serve them from their own replicated bolt state.

Three properties worth knowing:

- **One hop, never a cycle.** `Propose` is leader-only: a node that is not the leader refuses rather than passing the entry on.
- **Failures are distinguishable.** `ErrNoLeader` means an election is in progress (wait and retry); `ErrForwardUnavailable` means a leader exists but is unreachable (a wiring or network problem). The gateway degrades to its in-memory session buffer on either.
- **`Propose` grants no new authority.** Any cluster member can already replicate arbitrary log entries through the Raft transport on the same server under the same mTLS identity — the trust boundary is cluster membership, not this method. It must never be exposed outside the cluster mesh.

Some paths stay leader-only by design and do *not* forward: `Dream`, session pruning and the scheduler are singletons that should skip on a follower rather than relocate their work. `Forget` is also leader-only, because it scans for a matched set and then deletes it — forwarding each delete individually would run the scan against the follower's view while the deletes landed on the leader.

## Snapshot + restore

Snapshots are bolt-DB-style — periodically the FSM writes the current store state to a snapshot stream. Restore replaces the store entirely.

Services retain a `*Store`, whose atomic database pointer is replaced only after a new snapshot has been staged, synced, validated and opened. Preparation failures leave the original store usable. A lifecycle mutex serializes restores and shutdown; repeated restores always target the canonical `state.db` path.

Before replacing the canonical file, restore creates and syncs a sibling recovery hard link (`state.db.restore-*.previous`). If syncing the installed snapshot fails, it rolls back to the original file. A failed rollback disables the store and shuts down Raft and the owning node rather than continuing with ambiguous disk state. This requires a filesystem supporting same-directory hard links, rename and directory sync.

After successful publication, cleanup failures are logged as warnings: the restore remains committed, including the FSM's last-applied cache reset. Active transactions on the old database drain before its handle closes. A caller that loaded the old handle but has not started its transaction may receive `database not open`; restore does not provide transaction leasing.

### Interrupted restore recovery

Startup refuses to open a database while a matching `.previous` recovery file exists. This can indicate an interrupted restore, a failed rollback, or incomplete cleanup after a committed restore. Do not delete the recovery file merely to bypass the check.

Stop the node and retain copies of the canonical database and all `state.db.restore-*` files. Inspect the logged restore error, the candidate and previous databases, and their last-applied indexes against the node's Raft snapshot/log metadata. Recover a consistent database and Raft state together, or rebuild the affected peer from a healthy cluster using the normal recovery procedure. Only remove the recovery marker after resolving that consistency check. The filenames alone do not identify which state Raft committed.

## Leader election + failover

Standard raft semantics. Loss of quorum stalls writes; reads continue. New leader picks up scheduler firing where the old left off (idempotent — `last_run` in the task record prevents double-fire).

## Cluster size

The common deployments:

- **1 node** — the typical personal-assistant case. Raft is still the data path (single-member quorum); every entry commits as soon as it's written to disk. You lose fault tolerance — if the node dies, the cluster is offline — but that's true of any single-host deployment, and the simplicity is worth it for most operators.
- **3 nodes** — fault-tolerant. Tolerates 1 failure. Recommended when you care about uptime.
- **5 nodes** — tolerates 2 failures. Useful for large home-lab / small-team setups.

Less useful:

- **2 nodes** — no quorum tolerance (loss of either is loss of consensus). Run 1 or 3, not 2.
- **4 nodes** — same fault tolerance as 3, more network chatter. Pick 3 or 5.
- **7+** — raft starts to bottleneck on log replication latency. Rare for personal-assistant workloads.

When running multi-node, keep them on the same LAN. Cross-region adds 50-100ms to every write.

## Reference

- `internal/memory/raft.go` — raft setup, transport, snapshot config
- `internal/memory/fsm.go` — Apply + Snapshot + Restore
- `internal/memory/store.go` — bolt + atomic.Pointer
- `pkg/proto/lobslaw/v1/lobslaw.proto` — `LogEntry`
- [hashicorp/raft README](https://github.com/hashicorp/raft) for protocol details
