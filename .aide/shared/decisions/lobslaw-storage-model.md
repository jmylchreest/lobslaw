---
topic: lobslaw-storage-model
decision: "Storage is a first-class node function (fifth, alongside memory/policy/compute/gateway). A storage-enabled node manages filesystem mounts for that node. Three backend types in MVP: local: (bind mount), nfs: (kernel NFS mount via `mount -t nfs` subprocess), rclone: (rclone mount subprocess via FUSE — covers S3/R2/GCS/Azure/SFTP/WebDAV/etc). Mount config lives cluster-wide in Raft; every storage-enabled node materialises the config locally within its own mount namespace. Memory-enabled nodes must also enable storage so snapshot-export targets are resolvable (enforced at startup). Change detection uses a unified storage.Watcher: fsnotify for local-origin writes + periodic re-scan for remote-origin writes (default 5m on nfs/rclone, disabled on local). Pure-Go S3 SDK mode is explicitly NOT in scope — use cases are filesystem-oriented (git clone, edit code, skills hot-reload) and need POSIX paths. Cross-cluster storage routing/tunneling is deferred post-MVP"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-storage-model

**Decision:** Storage is a first-class node function (fifth, alongside memory/policy/compute/gateway). A storage-enabled node manages filesystem mounts for that node. Three backend types in MVP: local: (bind mount), nfs: (kernel NFS mount via `mount -t nfs` subprocess), rclone: (rclone mount subprocess via FUSE — covers S3/R2/GCS/Azure/SFTP/WebDAV/etc). Mount config lives cluster-wide in Raft; every storage-enabled node materialises the config locally within its own mount namespace. Memory-enabled nodes must also enable storage so snapshot-export targets are resolvable (enforced at startup). Change detection uses a unified storage.Watcher: fsnotify for local-origin writes + periodic re-scan for remote-origin writes (default 5m on nfs/rclone, disabled on local). Pure-Go S3 SDK mode is explicitly NOT in scope — use cases are filesystem-oriented (git clone, edit code, skills hot-reload) and need POSIX paths. Cross-cluster storage routing/tunneling is deferred post-MVP

## Rationale

Earlier model put rclone mounts under the compute node, which broke snapshot-export for memory-only nodes and couldn't express cluster-wide mount config propagation. Promoting storage to a first-class function gives: (1) memory nodes that can snapshot-export, (2) mount config in Raft so adding a mount propagates everywhere, (3) clean separation from compute's agent-loop concerns. Use cases (git clone, code editing, skills sharing with hot-reload) demand filesystem mounts, not API clients — so pure-Go S3 is not useful as a mount replacement. Unified Watcher with fsnotify+poll lets us detect changes deterministically without depending on rclone's or NFS's FUSE notification behaviour (which is unreliable for backend-origin writes)

