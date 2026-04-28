---
sidebar_position: 6
---

# Storage

Mounts, the watcher, the bolt files on disk.

## Layout

```
<data_dir>/
  state.db                   ← bolt store (raft FSM target)
  raft/
    raft.db                  ← raft log + stable store
    snapshots/               ← raft snapshot dir

<workspace>/                  ← user-facing files; see storage mounts
  incoming/
    <turn_id>/
      image_001.png           ← downloaded attachments
      ...

<skill-tools mount>/          ← installed skills
  gws-workspace/
    manifest.yaml
    bin/
      gws-workspace
  ...

audit/
  audit-2026-04-28.jsonl      ← daily audit log

certs/
  ca.pem
  node.pem
  node-key.pem
```

## state.db (bolt)

The single bolt file all the FSM-managed state lives in. Buckets per-domain (see [Memory](/architecture/memory) for the list).

Operations:

- Reads — direct, lock-free, fast.
- Writes — only the raft leader's FSM Apply path writes. Followers receive applies via raft log replication.

Snapshot writes serialize the bolt file via `bolt.Tx.WriteTo(w)`, raft streams it to followers on demand.

## raft.db

The raft log + stable store. Separate from `state.db` so raft's append pattern doesn't fragment the FSM bolt file.

## File watcher

`internal/storage/watcher.go` runs a fsnotify watch on the configured discover paths (typically `skill-tools` mount). When a `manifest.yaml` appears or changes:

```
fsnotify event: CREATE /var/lib/lobslaw/skills/my-skill/manifest.yaml
  ─► skills.ParseWithPolicy(manifest)
  ─► registry.RegisterExternal(tool)
  ─► tool now visible to agent
```

When a manifest disappears:

```
fsnotify event: REMOVE /var/lib/lobslaw/skills/my-skill/manifest.yaml
  ─► registry.UnregisterByPath("skill://my-skill/...")
  ─► tools no longer visible
```

The watcher is per-node (manifests live on local disk, not raft). For consistent cross-cluster skill availability, the operator either:

- Mounts the skill-tools volume from shared storage, or
- Installs the same skills on every node (the install pipeline is operator-driven, so this is straightforward).

## Inspect

`cmd/inspect/main.go` opens a stopped node's `state.db` and dumps records. Useful for forensics or debugging without needing to spin up the full cluster:

```bash
inspect --state-db /var/lib/lobslaw/data/state.db --memory-key-ref env:LOBSLAW_MEMORY_KEY \
        --bucket commitments --limit 20
```

## Reference

- `internal/memory/store.go` — bolt wrapper
- `internal/memory/raft.go` — raft setup
- `internal/storage/watcher.go` — manifest watcher
- `cmd/inspect/main.go` — offline inspection tool
