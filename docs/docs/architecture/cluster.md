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

Only the Raft leader's services accept mutation calls. Followers either:

- Forward to the leader (gateway → cluster gRPC).
- Or return a "not leader" error and let the caller retry against the leader.

Most code goes through `NodeService.Apply(ctx, entry)` which transparently forwards. Reads are always local.

## Snapshot + restore

Snapshots are bolt-DB-style — periodically the FSM writes the current store state to a snapshot stream. Restore replaces the store entirely.

The tricky part: at boot or post-snapshot-restore, *outside references* to the bolt handle (held by policy engine, scheduler, services) are still pointing at the old `*bolt.DB`. If we close-and-reopen, those references are dangling.

Fix: `internal/memory/store.go` wraps the bolt handle in `atomic.Pointer[bolt.DB]`:

```go
type Store struct {
    db   atomic.Pointer[bolt.DB]
    path string
    key  crypto.Key
}

func (s *Store) RestoreFromSnapshot(rc io.ReadCloser) error {
    newDB, err := openFromSnapshot(rc, s.path, s.key)
    if err != nil { return err }
    old := s.db.Swap(newDB)
    if old != nil { _ = old.Close() }
    return nil
}
```

Outside refs hold `*Store`, not `*bolt.DB`. They call `s.loadDB().View(...)` per access; the pointer is always live.

This was a real bug — pre-fix, snapshot restore cascaded "database not open" errors across every service for ~5 seconds until they all gave up. Now it's seamless.

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
