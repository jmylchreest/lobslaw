---
sidebar_position: 3
---

# Memory

The bolt store, the buckets, the encryption.

## Buckets

| Bucket | Schema | Encryption |
|---|---|---|
| `episodic` | `EpisodicRecord` | none (operator data, not credentials) |
| `soul_fragments` | `SoulFragment` | none |
| `scheduled_tasks` | `ScheduledTask` | none |
| `commitments` | `AgentCommitment` | none |
| `policy_rules` | `PolicyRule` | none |
| `credentials` | `CredentialRecord` | AES-256-GCM via `crypto.Seal` |
| `credential_acls` | `CredentialACL` | none (the ACL is metadata; credential is encrypted) |
| `user_prefs` | `UserPreferences` | none |
| `dreams` | `DreamRecord` | none |
| `embeddings` | dense vectors keyed by record id | none (the underlying record is) |

Why most aren't encrypted: the threat model assumes a peer holding the cluster MemoryKey is fully trusted (otherwise the cluster is compromised). Encrypting episodic memory protects against bolt-file theft from a stopped node, but the value is marginal compared to the cost in operability. **Credentials are the exception** because the cost of leakage is much higher.

## Atomic.Pointer pattern

Discussed in [Cluster](/architecture/cluster) — `*bolt.DB` lives behind `atomic.Pointer` so snapshot restore can swap in place without invalidating outside refs.

## Schema migrations

There is no formal migration framework. Bolt is schemaless from its perspective; we own the marshalling. Schema changes happen by:

1. Add the new field to the proto.
2. Marshal new records with the field set; old records' missing field defaults to zero.
3. If the new field requires a backfill, write a one-shot tool in `cmd/` that reads old records and writes them with the field populated.

For the embedding model change case specifically, `cmd/backfill-embeddings/main.go` re-embeds every episodic record in place.

## Encryption

```go
sealed, _ := crypto.Seal(memoryKey, plaintext)
// sealed is: nonce(12) || ciphertext || tag(16)
```

`memoryKey` is provisioned at cluster bootstrap. Identical on every peer (so any peer can decrypt). Single rotation today is "stop the cluster, re-encrypt every credential record with a new key, restart" — there's no online rotation surface. Roadmap.

## Per-record nonce

`crypto.Seal` generates a fresh 12-byte nonce per call. Two seals of the same plaintext produce different ciphertext, so:

- No frequency analysis from an attacker reading raw bolt files.
- Replays are caught by the inner record's id + ts (a re-applied raft log entry is idempotent).

## Reference

- `internal/memory/store.go` — bolt wrapper, atomic.Pointer, restore
- `internal/memory/buckets.go` — bucket name constants
- `internal/memory/credentials.go` — encrypted-at-rest CRUD
- `pkg/crypto/seal.go` — AEAD wrapper
