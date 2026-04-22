---
topic: lobslaw-trust-model
decision: "Single-tenant cluster by design. JWT scope is a routing and audit-attribution label (picks soul, tags audit, RBAC per user), NOT a confidentiality boundary. All memory sits in one trust pool. True multi-tenant isolation requires a second cluster, not a retrofit"
date: 2026-04-22
---

# lobslaw-trust-model

**Decision:** Single-tenant cluster by design. JWT scope is a routing and audit-attribution label (picks soul, tags audit, RBAC per user), NOT a confidentiality boundary. All memory sits in one trust pool. True multi-tenant isolation requires a second cluster, not a retrofit

## Rationale

Real multi-tenant confidentiality needs per-scope encryption keys, scope-enforced Recall, crypto-isolated partitions - too much design weight for a personal/household assistant. Keeping scope logical preserves per-user RBAC and audit attribution without the crypto burden

