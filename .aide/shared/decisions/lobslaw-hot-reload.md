---
topic: lobslaw-hot-reload
decision: "Most config and all skills/plugins hot-reloadable via copy-on-write registries + fsnotify watcher + NodeService.Reload RPC. Every turn/invocation captures a config snapshot at start so in-flight work is unaffected. Restart-required list is minimal: node id, raft_port, gRPC listener ports, and (for now) mTLS cert rotation. Skill and plugin hot-reload are in MVP acceptance; mTLS cert hot-swap is future work"
date: 2026-04-22
---

# lobslaw-hot-reload

**Decision:** Most config and all skills/plugins hot-reloadable via copy-on-write registries + fsnotify watcher + NodeService.Reload RPC. Every turn/invocation captures a config snapshot at start so in-flight work is unaffected. Restart-required list is minimal: node id, raft_port, gRPC listener ports, and (for now) mTLS cert rotation. Skill and plugin hot-reload are in MVP acceptance; mTLS cert hot-swap is future work

## Rationale

Architecture already favours hot-reload - skills are directories on mounted storage, hooks are subprocess-per-event, policy is Raft-backed. CoW snapshots keep in-flight work correct across reloads without locks. Making skill/plugin hot-reload MVP is a big usability win ('drop a skill in and use it') and costs ~day of work given the existing structure

