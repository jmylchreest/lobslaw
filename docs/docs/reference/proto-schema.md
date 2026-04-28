---
sidebar_position: 3
---

# Proto Schema

The full canonical proto schema lives at `pkg/proto/lobslaw/v1/lobslaw.proto`. This page is a high-level index — read the proto file for exact field names and tags.

## Top-level shapes

| Message | Purpose |
|---|---|
| `LogEntry` | One Raft log payload — has a `oneof` over every state mutation type |
| `EpisodicRecord` | A single conversational/tool event |
| `SoulFragment` | Operator-tunable agent persona attribute |
| `ScheduledTask` | Cron-style recurring task |
| `AgentCommitment` | One-shot future agent turn |
| `PolicyRule` | Single allow/deny/require-confirmation rule |
| `CredentialRecord` | OAuth credential (encrypted-at-rest payload) |
| `CredentialACL` | Skill ↔ credential ↔ scope mapping |
| `UserPreferences` | User profile: display name, timezone, channels |
| `UserChannelAddress` | One channel binding inside `UserPreferences.channels` |
| `DreamRecord` | Synthesised dream summary |

## LogEntry

Every Raft log entry is a `LogEntry`:

```protobuf
message LogEntry {
  oneof payload {
    EpisodicRecord    episodic        = 1;
    SoulFragment      soul_fragment   = 2;
    ScheduledTask     scheduled_task  = 3;
    AgentCommitment   commitment      = 4;
    PolicyRule        policy_rule     = 5;
    CredentialRecord  credential      = 6;
    CredentialACL    credential_acl   = 7;
    DreamRecord       dream           = 8;
    // ...
    UserPreferences   user_prefs      = 21;
  }
}
```

The FSM `Apply` switches on this oneof and dispatches to the right service.

**Numbered tags are stable.** Adding new fields uses the next free tag; never reuse retired tags. (We retired `TrustedPublisherKey`'s tag when we removed the message; it's not reused.)

## Encryption

`CredentialRecord.payload` is `bytes` — opaque to the proto layer. The bytes are the output of `crypto.Seal(memoryKey, plaintextStruct)`, which is itself a marshalled inner struct with access_token, refresh_token, scopes, expires_at.

Other fields on `CredentialRecord` (subject, provider, expires_at, scopes) are duplicated outside the encrypted payload for indexing — minor redundancy in exchange for being able to search/list without decrypting.

## Versioning

The package is `lobslaw.v1`. A `v2` would be added side-by-side; we don't break v1 wire format. Migrations happen at the marshalling layer (writers can produce either; readers handle both for a deprecation window).

## Where to look

- **Schema**: `pkg/proto/lobslaw/v1/lobslaw.proto`
- **Generated code**: `pkg/proto/lobslaw/v1/lobslaw.pb.go`
- **Service consumers**: `internal/memory/*.go` — one file per major message type, with `Apply` + getter logic
