---
topic: lobslaw-fs-mount-scoping
decision: "Fs builtins accept mount-scoped paths ('workspace/notes.md') in addition to raw absolute paths. MountResolver translates label/subpath → absolute path with traversal + writable + exclude enforcement. StorageMountConfig gains Writable bool (default false) + Excludes []string fields. Absolute paths continue to work back-compat but are discouraged in operator docs. Configured via [[storage.mounts]] with writable=true to allow write/edit."
date: 2026-04-24
---

# lobslaw-fs-mount-scoping

**Decision:** Fs builtins accept mount-scoped paths ('workspace/notes.md') in addition to raw absolute paths. MountResolver translates label/subpath → absolute path with traversal + writable + exclude enforcement. StorageMountConfig gains Writable bool (default false) + Excludes []string fields. Absolute paths continue to work back-compat but are discouraged in operator docs. Configured via [[storage.mounts]] with writable=true to allow write/edit.

## Rationale

Bot was fabricating paths like /var/lobslaw/workspace (which happened to match the old hardcoded promptgen default). Mount-scoping forces path intent to match a configured mount — the LLM either uses a known label or hits an actionable error listing available mounts. Writable-default-false prevents accidental overlap with cluster-private dirs (Raft snapshots, bbolt files). Excludes lets operators carve out subtrees (.git, node_modules) even within allowed mounts.

