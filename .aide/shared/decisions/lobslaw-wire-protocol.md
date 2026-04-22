---
topic: lobslaw-wire-protocol
decision: "Pure gRPC with typed services (NodeService, MemoryService, PolicyService, AgentService, ChannelService, PlanService, AuditService). mTLS mandatory for all cluster-internal gRPC. No custom Message/Envelope wrapper - gRPC metadata carries trace ids, deadlines carry TTL. External gateway TLS is separate from cluster mTLS"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-wire-protocol

**Decision:** Pure gRPC with typed services (NodeService, MemoryService, PolicyService, AgentService, ChannelService, PlanService, AuditService). mTLS mandatory for all cluster-internal gRPC. No custom Message/Envelope wrapper - gRPC metadata carries trace ids, deadlines carry TTL. External gateway TLS is separate from cluster mTLS

## Rationale

Original decision introduced a custom Message type with invoke/response/subscribe/publish verbs. That duplicates what gRPC already provides (method dispatch, deadlines, metadata) and adds a dead layer. Pure gRPC is simpler and idiomatic. mTLS is now explicit and mandatory per lobslaw-encryption

