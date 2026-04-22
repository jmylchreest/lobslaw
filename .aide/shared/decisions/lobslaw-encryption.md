---
topic: lobslaw-encryption
decision: "At-rest: boltdb values wrapped with nacl/secretbox using an operator-provided key (env/KMS ref); rclone crypt as a first-class option on [[storage.mounts]]. In-transit: mTLS mandatory for all cluster-internal gRPC (Raft, memory, policy, audit); external gateway TLS is separate. JWT default is RS256/EdDSA via JWKS; HS256 only as a single-node fallback"
date: 2026-04-22
---

# lobslaw-encryption

**Decision:** At-rest: boltdb values wrapped with nacl/secretbox using an operator-provided key (env/KMS ref); rclone crypt as a first-class option on [[storage.mounts]]. In-transit: mTLS mandatory for all cluster-internal gRPC (Raft, memory, policy, audit); external gateway TLS is separate. JWT default is RS256/EdDSA via JWKS; HS256 only as a single-node fallback

## Rationale

Raft+boltdb holds an episodic diary of your life - plaintext-on-disk is unacceptable for a personal agent. rclone crypt stops backend buckets being readable by anyone with bucket access. mTLS inside the cluster stops network-adjacent attackers writing to memory. Symmetric JWT means a single secret leak compromises every verifier - default asymmetric

