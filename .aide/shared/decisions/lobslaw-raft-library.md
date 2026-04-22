---
topic: lobslaw-raft-library
decision: "hashicorp/raft with a custom gRPC Transport implementation (seeded from Jille/raft-grpc-transport). Raft log via hashicorp/raft-boltdb (a ~300-LOC adapter over go.etcd.io/bbolt - bbolt is the single storage engine). All pure Go (CGO_ENABLED=0 preserved per go-cgo decision). Re-affirmed after considering a pivot to go.etcd.io/raft/v3 for aide-alignment; the alignment argument collapsed when we confirmed aide uses bbolt but not raft, so there is no raft-layer code to share, and hashicorp/raft-boltdb is already bbolt underneath"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-raft-library

**Decision:** hashicorp/raft with a custom gRPC Transport implementation (seeded from Jille/raft-grpc-transport). Raft log via hashicorp/raft-boltdb (a ~300-LOC adapter over go.etcd.io/bbolt - bbolt is the single storage engine). All pure Go (CGO_ENABLED=0 preserved per go-cgo decision). Re-affirmed after considering a pivot to go.etcd.io/raft/v3 for aide-alignment; the alignment argument collapsed when we confirmed aide uses bbolt but not raft, so there is no raft-layer code to share, and hashicorp/raft-boltdb is already bbolt underneath

## Rationale

Considered pivoting to etcd-io/raft on the grounds of 'same team as bbolt' and aide alignment. Both arguments collapse: aide doesn't use raft at all (only bbolt), and hashicorp/raft-boltdb IS bbolt via a thin adapter. Meanwhile etcd-io/raft requires writing ~3k LOC of FSM wrapper + snapshot streaming + transport that hashicorp/raft bundles. For lobslaw's personal-scale (1-3 nodes), hashicorp/raft's scale headroom is ample. Net: ~2 weeks of Phase 2 saved, one library to own

