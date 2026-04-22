---
topic: lobslaw-storage-model
decision: "StorageNode runs rclone mount as subprocess, jailed in mount namespace. No CSI, no orchestrator integration. Container just sees /cluster/store/... as local paths after rclone mounts them. Infrastructure concern is just ensuring FUSE support in the container/pod"
date: 2026-04-22
---

# lobslaw-storage-model

**Decision:** StorageNode runs rclone mount as subprocess, jailed in mount namespace. No CSI, no orchestrator integration. Container just sees /cluster/store/... as local paths after rclone mounts them. Infrastructure concern is just ensuring FUSE support in the container/pod

## Rationale

Simple, self-contained. rclone is well-maintained and production-proven. No k8s operators or special orchestration. The mount is simply there when the process starts, same as any filesystem

