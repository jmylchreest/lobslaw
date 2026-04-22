---
topic: lobslaw-storage-layer
decision: "Object store with nested/mountable sandboxed filesystem. Backend: local disk and/or S3 (MinIO, S3, R2). StorageNodes provide shared clustered disk access. Skills stored in storage, merged in configured order"
date: 2026-04-22
---

# lobslaw-storage-layer

**Decision:** Object store with nested/mountable sandboxed filesystem. Backend: local disk and/or S3 (MinIO, S3, R2). StorageNodes provide shared clustered disk access. Skills stored in storage, merged in configured order

## Rationale

Object store as primary storage abstraction; sandboxed FS for agent safety; S3 compatibility allows R2/MinIO/cloud flexibility

