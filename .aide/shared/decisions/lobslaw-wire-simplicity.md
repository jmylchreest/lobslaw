---
topic: lobslaw-wire-simplicity
decision: "gRPC for agent-to-agent sync and service calls. No NATS for now. If pub/sub/eventing needed later, revisit. Raft+direct gRPC sufficient for MVP"
date: 2026-04-22
---

# lobslaw-wire-simplicity

**Decision:** gRPC for agent-to-agent sync and service calls. No NATS for now. If pub/sub/eventing needed later, revisit. Raft+direct gRPC sufficient for MVP

## Rationale

Simplicity over features. gRPC covers most needs. Raft handles cluster consensus. Add NATS only when justified

