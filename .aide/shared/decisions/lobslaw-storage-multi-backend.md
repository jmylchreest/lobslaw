---
topic: lobslaw-storage-multi-backend
decision: "Any number of storage backends (local dirs, S3, MinIO, R2) with priority-based merge. Highest priority wins per-path. Path prefix routing allows logical namespaces. No required minimum"
date: 2026-04-22
---

# lobslaw-storage-multi-backend

**Decision:** Any number of storage backends (local dirs, S3, MinIO, R2) with priority-based merge. Highest priority wins per-path. Path prefix routing allows logical namespaces. No required minimum

## Rationale

Real deployments need multiple tiers (fast SSD, cold S3, R2 backup). Priority merge is simpler than complex routing rules and handles all common cases

