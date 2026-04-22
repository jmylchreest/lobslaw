---
topic: lobslaw-raft-library
decision: "hashicorp/raft with a custom gRPC Transport implementation (seeded from Jille/raft-grpc-transport). Raft log in hashicorp/raft-boltdb (separate bbolt file from application state). Rejected: etcd-io/raft (would require ~3k LOC of transport + FSM wrapper + snapshot streaming code to match what hashicorp/raft bundles). Rejected: hashicorp/raft + stock NewTCPTransport (two transports, two ports, two mTLS configs, two cert-rotation stories)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-raft-library

**Decision:** hashicorp/raft with a custom gRPC Transport implementation (seeded from Jille/raft-grpc-transport). Raft log in hashicorp/raft-boltdb (separate bbolt file from application state). Rejected: etcd-io/raft (would require ~3k LOC of transport + FSM wrapper + snapshot streaming code to match what hashicorp/raft bundles). Rejected: hashicorp/raft + stock NewTCPTransport (two transports, two ports, two mTLS configs, two cert-rotation stories)

## Rationale

The architectural win we actually want from this choice is unified mTLS transport + peer identity from cert SAN + gRPC interceptor observability applying to Raft traffic. That's a transport-layer concern. Custom gRPC Transport over hashicorp/raft gets it in ~300-500 LOC while keeping hashicorp's mature FSM/snapshot/joint-consensus machinery. Net vs etcd/raft: ~2 weeks of Phase 2 saved. Net vs stock hashicorp transport: one cert rotation story, one port, one observability pipeline

