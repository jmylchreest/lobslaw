---
type: memories
category: decision
count: 2
exported: 2026-04-22
---

# Decision

### SUPERSEDED: lobslaw-memory-layer decision (2026-04-22) was updated on 2026-04...

<!-- aide:id=01KPT5EW8RD5VKQPGQNPCEB2ZF tags=project:lobslaw,source:user,raft,storage,dependency,superseded date=2026-04-22 -->

SUPERSEDED: lobslaw-memory-layer decision (2026-04-22) was updated on 2026-04-22. New stack: go.etcd.io/raft/v3 + go.etcd.io/bbolt (not hashicorp/raft + etcd-io/bbolt). Rationale: etcd-io/bbolt is what ../aide uses for storage; etcd-io/raft is the battle-tested Raft used by etcd/k8s/cockroachdb; both from same team and compose cleanly. hashicorp/raft was abandoned in favour of this stack. Membership changes (add/remove node) are now confirmed as MVP scope - hashicorp/raft had this built-in and etcd-io/raft supports it via ProposeConfChange/ApplyConfChange.

---

### Use go.etcd.io/raft/v3 + go.etcd.io/bbolt (pure Go, no CGO). etcd-io/raft has...

<!-- aide:id=01KPT64J4CMHSXN0APTZ2X4VQT tags=project:lobslaw,source:discovered,raft,transport,membership,verified:true date=2026-04-22 -->

Use go.etcd.io/raft/v3 + go.etcd.io/bbolt (pure Go, no CGO). etcd-io/raft has no built-in network transport — lobslaw implements a custom gRPC-based Transporter using the cluster's existing mTLS connections. This is MVP scope. Membership changes (add/remove node while running) are also MVP via ProposeConfChange/ApplyConfChange.

---

