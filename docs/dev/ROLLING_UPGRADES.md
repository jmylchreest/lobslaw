# Design: coordinated activation for rolling binary upgrades

Status: implementation in progress. The initial transition from unversioned deployments is coordinated; rolling upgrades start with the control-protocol baseline in this branch.

## Overview

Separate a node's supported capabilities from the cluster's active data contract. Nodes can be replaced one at a time while all writers, snapshot producers and feature handlers remain on the committed active contract. A separate operator-controlled activation advances that contract after every configured member is ready.

The first release must teach every node this protocol. Existing unversioned deployments cannot be assumed to understand activation, reject future state, or correctly decode historical colliding tags. Default proposal: one coordinated transition to the baseline, then rolling upgrades between explicitly tested adjacent contracts.

## Interfaces

Proposed neutral types in internal/dataformat (no storage, transport or feature imports):

```go
type ContractID uint32
type Capabilities struct {
    ControlProtocol uint32
    ReadContracts []ContractID
    WriteContracts []ContractID
    SnapshotContracts []ContractID
}
type ActiveContract struct {
    Contract ContractID
    Epoch uint64
}
type Transition struct {
    ID string
    From ActiveContract
    Target ContractID
    MembershipIndex uint64
    MemberIDs []string
}
```

Use explicit tested contracts, not an assumption that all integers between a minimum and maximum are compatible. Each contract identifies log payload semantics, snapshot layout and available features. Local physical formats are tracked separately; a rolling binary update must not automatically make its directory unreadable by the previous binary if rollback is promised.

Authenticated NodeService RPCs expose capabilities, active epoch and upgrade status. Operator-only status/prepare/finalize commands follow existing authorization. Capabilities identify the actual peer, boot incarnation and membership, not arbitrary discovery advertisements. Never expose activation as an agent tool or grant authority through the compatibility header.

## Data flow

1. Install baseline protocol support through the supported initial transition.
2. Replace followers individually; require catch-up and healthy quorum before proceeding. Transfer leadership to a caught-up upgraded voter before replacing the leader.
3. All versions continue writing the committed old contract. New features remain disabled; a new leader is not permission to switch formats.
4. Leader plans activation against the authoritative Raft configuration and probes all configured members, including non-voters if later supported.
5. A committed prepare record freezes membership changes for this transition. Every member durably installs a target-version restart fence and acknowledges the exact transition and membership generation after applying the prepare index. An offline member blocks finalization; removing it is a separate explicit operation before preparation.
6. A committed finalize record advances the active contract at one log index. Epoch checks, membership serialization and leader-change recovery prevent stale probes or a concurrent AddVoter from authorizing activation. Duplicate prepare/finalize requests are idempotent; status resolves a lost response.
7. Contract migration and epoch publication must be atomic and crash recoverable. New-format proposals cannot overtake activation. Snapshot output includes the committed contract and transition state. An incoming snapshot is accepted only if supported; replay retains the contract transition order.
8. A binary below the durable restart fence cannot rejoin after activation. Abort semantics before finalization must be explicitly defined; after activation, restoring the original cluster backup is recovery, not an online downgrade.

## Key decisions

| Decision | Choice | Rationale |
| --- | --- | --- |
| Bootstrap | Coordinated first transition by default | Existing binaries do not implement this protocol |
| Activation | Explicit, replicated operation | Binary rollout and irreversible data changes are separate |
| Readiness | Every configured member, current membership generation | A temporarily offline voter must not be silently abandoned |
| Contract | Explicit semantic compatibility table | A protobuf envelope number does not establish feature compatibility |
| Snapshots | Emit active contract throughout mixed-version rollout | Old followers may need snapshot catch-up |
| Rollback | Tested only before activation with compatible local storage | Current eager startup migration would otherwise prevent rollback |
| Authentication | Existing peer/operator boundaries | Version negotiation is not authorization |
| Historical data | Existing explicit provenance profiles | Negotiation cannot resolve tag collisions in old logs |

## Acceptance criteria

- Real three-node mixed-binary tests preserve writes across follower replacement and leader transfer, subject to quorum; two-node clusters cannot tolerate one voter being stopped.
- Old and new leaders both emit the active old contract before finalization.
- New-only commands are refused before proposal, including raw forwarded Propose; they never commit and halt an older FSM.
- An old follower can install a snapshot produced by a newer node during the mixed-version phase.
- All configured members must apply prepare and persist the restart fence before finalize; stale sessions, unknown capabilities, missing members and incompatible members block activation.
- Membership changes, concurrent operators and leader failure cannot race activation. Prepared state survives snapshots and restart.
- Crashes before/after prepare, fence persistence, finalize and migration publication are recoverable without falsely advancing the active contract.
- A rolled-back binary can restart before activation only within its documented local-format compatibility; after activation it refuses before mutation or joining.
- Lost RPC responses and retries never activate twice or silently perform membership changes.
- Existing ownership, permission and external-effect recovery semantics are unchanged.
- Upgrade status reports active/target contracts and actionable blockers without leaking secrets.
- Explicit compatibility fixtures and CI matrix cover every advertised supported release pair; unsupported skipped releases fail closed.

## Files to create/modify

- internal/dataformat: pure contract catalogue, compatibility decisions and transition validation.
- pkg/proto/lobslaw/v1/lobslaw.proto: stable control messages, capabilities/status RPCs and replicated transition records; regenerate Go/browser bindings as applicable.
- internal/memory: persistent active contract/transition/fence state; serialized proposal admission and membership operations; atomic activation; snapshot contract handling.
- internal/grpcinterceptors: replace exact binary protocol equality with stable control handshake plus active-contract compatibility. Admission must account for both sides and restore/catch-up state.
- internal/discovery and internal/node: authenticated capability exchange, identity binding, readiness, membership fencing and lifecycle wiring.
- cmd/lobslaw: operator upgrade status, prepare/finalize and explicit abort handling.
- docs: operator runbook, supported pairs, quorum requirements, rollback boundary and initial transition limits.
- #348 layer: team payload gates, lazy/contract-specific bucket creation and snapshots, main-contract operation while teams remain inactive.
- Integration tests: actual baseline and target binaries, snapshots, elections, offline members, membership races and crash injection.

## Dependencies

Use the existing HashiCorp Raft, bbolt, protobuf, mTLS and operator authorization mechanisms. No new consensus algorithm. Validate the actual transport and snapshot lifecycle against pinned dependencies during implementation.

## Delivery

1. Define and test the baseline control contract and durable transition state.
2. Implement command/snapshot compatibility and membership/activation fencing; expose operator tooling.
3. Adapt #348 to remain on the main contract during rollout and activate team state explicitly.
4. Validate real mixed-version clusters and crash/restart scenarios before advertising rolling support.

Keep these reviewable commits or PRs above the migration foundation and below #348. Do not weaken the current exact-match guard until the whole path is enforced.

## Out of scope

Arbitrary historical mixed-tag reconciliation, automatic software deployment, unattended destructive membership changes, guaranteed uninterrupted service without quorum, automatic downgrade after contract activation, and an unbounded promise of rolling compatibility across all future versions.
