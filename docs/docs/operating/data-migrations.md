# Data upgrades and recovery

Lobslaw versions its state database, Raft command envelopes and portable archives
separately. Supported old data is validated and migrated; unknown newer formats
are refused. Do not downgrade an upgraded data directory. Keep its original backup
and matching binary for rollback.

## Container startup checks

The normal `lobslaw run` startup checks the mounted data, so a supported update
does not need a separate migration entrypoint. Checks use the **stored data
format and active contract**, rather than the image tag or release version.

Before opening the node for normal operation, startup validates the state
format, existing Raft commands and physical snapshots. Supported populated
unversioned state is upgraded through the existing state adapter. A private
`startup-backup-<id>/` directory is durably published in the data volume first,
containing the original `state.db`, any existing `raft.db`, snapshot repository,
and physical format manifest. Encrypted record and retained Raft/snapshot bytes
are preserved in that backup. If backup creation fails, migration does not start.

The startup log reports `stored_format`, `active_format`, `migrated`, and the
backup path. Starting again with current data does not repeat the migration or
create another startup backup. Empty volumes are initialized without a migration
backup. Cluster contract activation still follows the readiness process below.

Startup refuses unsupported formats, unknown buckets, incompatible snapshots,
and ambiguous historical logs before migration changes the state database. An
ambiguous legacy layout still requires the explicit offline procedure below;
startup cannot infer its provenance from an image tag. Restore-mode requirements
for copied recovery data continue to apply.

Startup backups are core persistence images, not backups of every mounted file:
retain the same encryption key, configuration, certificates, attachments, and
other machine-local files separately. Auxiliary files already in the live data
volume, including browser sessions and operator identity files, remain in place.
The `startup-backup.json` marker prevents a new container from booting a startup
backup as a duplicate live node. Use `data inspect`/`data migrate` explicitly for
recovery and keep the matching original binary.

## Physical data directories

Stop **all** Raft members for this upgrade. The first versioned persistence release
requires matching persistence protocols; mixed-version replication and incompatible
membership additions are rejected. A rejected compatibility check must not be
worked around by bootstrapping another cluster.

`lobslaw data inspect` performs offline preflight. `lobslaw data migrate` creates a
new directory from the same validated source. Neither command overwrites source
data. A mistyped source path does not create an empty database.

```sh
lobslaw data inspect --data-dir /srv/lobslaw --memory-key-ref env:LOBSLAW_MEMORY_KEY --legacy-format main-v0
lobslaw data migrate --data-dir /srv/lobslaw --output /srv/lobslaw-upgraded --memory-key-ref env:LOBSLAW_MEMORY_KEY --legacy-format main-v0
```

Select `main-v0` only for retained unversioned logs from main. Earlier #348 builds
have two supported layouts: `pr348-v0` (bot field 51) and `pr348-early-v0` (bot field
37). Those require the team-capable #348 binary. Fixtures pin the supported layouts
to commits `5f4e6c4`, `9c651d6` and `ad38c95`, respectively. Do not guess based on
whether a command accepts the input: some ID-only records are ambiguous. Mixed
histories that changed the meaning of a field need separate investigation and are
not automatically supported. The selected provenance survives later restarts.

The output preserves state.db, raft.db, snapshots and membership. Keep the original
memory key, configuration and certificates separately; filesystem attachments and
other machine-local files are not copied. Never run source and copied directories
as two instances of the same node. Allow space for the source, destination and
staging copies. Snapshot inspection also needs temporary disk space for a private
copy of the snapshot repository and one validation image, even for `inspect`.
An interrupted migration never activates a partial directory.

When an unversioned populated state database is upgraded, the destination also
retains an encrypted `state-before-migration-*.db` image created by the state
adapter. This is a state-only diagnostic/rollback image, not a complete node
backup: it does not include the matching Raft log or snapshots. Keep the original
source directory and matching binary for whole-node rollback. Budget disk space
for this additional state database; startup does not automatically select it.

Point the node at the output directory and enable `[memory] restore_mode = true`.
Review pending work: an old backup can predate a calendar write or another external
action that already succeeded. Stop the node again and explicitly acknowledge the
review before disabling restore mode:

```sh
lobslaw data accept-recovery --data-dir /srv/lobslaw-upgraded --memory-key-ref env:LOBSLAW_MEMORY_KEY --acknowledge-external-effects
```

This acknowledgement does not execute tasks or grant new permissions. It permits
the operator to resume normal operation using the preserved task state.

## Encrypted backup repository generations

`lobslaw backup restore` continues to restore portable knowledge archives. It does
not restore a physical Raft directory, credentials or cluster identity. Schema 1
archives are normalized to schema 2 in memory after full authentication, while
explicit source identity and owner mappings remain required. Original backup
files stay unchanged. Continue using the existing preview/apply workflow in
restore mode.

An unsupported source format, missing encryption key, unknown feature bucket or
ambiguous legacy log produces an error rather than a partial import or guessed
conversion. Retain the source and use a binary supporting that format.

## Rolling binary upgrades after the initial transition

The rolling-control baseline supports data contract 1. PR #348's binary supports
contracts 1 and 2. The earlier migration-only binary is not the rolling baseline:
move to a binary with `lobslaw-rolling-v1` through the coordinated procedure above.
Only explicitly supported contract combinations can run together.

During a supported rollout, replace one follower at a time, wait for catch-up,
and preserve a majority of healthy voters. Transfer leadership before replacing
the leader using `lobslaw cluster upgrade transfer --context prod --member NODE_ID`. A two-voter cluster cannot retain quorum while one member is stopped.
This feature does not install binaries or automatically remove unavailable members.

During rollout, binaries continue using the active contract, including snapshot
output. Once every configured member supports automatic activation, is ready and
has caught up, the leader automatically prepares and finalizes reviewed safe
transitions. Contract 1 → 2 is the first such transition. Each phase requires
30 seconds of stable readiness, checked every 5 seconds. There is no configuration
switch. Deployment tooling still installs binaries; Lobslaw advances its data contract.

Installing eligible binaries authorizes this automatic transition. The supported
old-binary rollback window ends at preparation, even before finalization. Unknown,
destructive or operator-input-dependent transitions do not activate automatically.
Use `cluster upgrade status` to inspect `automatic_targets`, `automatic_ready` and
`automatic_blocker`. Older binaries without those capabilities block automatic
activation even when they can read contract 2. Offline members and non-voters count.
Restore mode suppresses the controller.

Manual control remains available for recovery. A verified operator certificate whose
identity has the configured `operator` role receives `cluster.upgrade.read` and
`cluster.upgrade.write` on `cluster:*` by default, including on memory-only nodes.
No extra allow rules are needed. The grants are separate policy fallbacks: matching
stored rules take precedence over them, using the normal policy priority order.
A role claim or JWT alone cannot authorize these RPCs. Peer credentials can inspect
local upgrade status but cannot request activation. These actions are not agent tools.
Every mutation still requires an audit admission record and the upgrade safety checks.

For example, keep operator status access but disable upgrade mutations:

```toml
[[policy.rules]]
id = "deny-operator-cluster-upgrade-write"
subject = "role:operator"
action = "cluster.upgrade.write"
resource = "cluster:*"
effect = "deny"
priority = 100
```

Use `subject = "user:alice"` to restrict one operator instead. Writes include
prepare, finalize, abort and leadership transfer. A read deny does not implicitly
deny writes, or vice versa; deny both actions to restrict both permissions. Existing
explicit grants continue to work; choose a deny priority above any matching explicit
allow rule. During a mixed-version rollout, older binaries still need explicit
grants, so retain those grants until every serving node has the new defaults.
These rules govern operator requests. The automatic controller uses separate,
code-defined cluster authority and is not disabled by an operator-role deny.


```sh
lobslaw cluster upgrade status --context prod
lobslaw cluster upgrade prepare --context prod --id teams-2026 --target 2 --epoch 0
lobslaw cluster upgrade finalize --context prod --id teams-2026 --target 2 --epoch 0
```

Send changes to the leader address reported by status. Use the observed epoch,
not a guessed value. Prepare checks every configured member and commits a durable
transition that freezes membership. Finalize requires every member to have durably
applied that exact preparation; retry after lagging members catch up. Missing or
old members block activation even if the cluster has quorum. A restarted node must
support the prepared target. Keep upgraded binaries installed once preparation
begins. A lost response is resolved using status and the same transition ID.

To abandon a prepared transition while retaining the old active contract:

```sh
lobslaw cluster upgrade abort --context prod --id teams-2026 --target 2 --epoch 0
```

Aborting an automatic transition leaves a durable hold on its target: the
controller will not undo the abort. Resume by completing the manual prepare/finalize
sequence with a fresh ID and the observed epoch. The `auto-contract-v1-` ID namespace
is reserved; choose a different ID for manual operations. The controller also leaves
manual preparations for the operator to finish.

Abort still requires quorum and the exact transition. It advances the epoch;
future attempts need a new ID. Do not treat abort as permission to downgrade local
data: prepared history may remain in logs/snapshots. Supported binary rollback is
before preparation. After activation, older binaries are refused; use the original
cluster backup and binary for disaster recovery, accounting for subsequent writes
and external effects. Do not start copied directories as duplicate node identities.

Team-capable nodes started on contract 1 keep team execution/tools disabled. After
finalization, configured team services become available in the same processes.
The inbox worker observes activation on its next wake or idle tick (at most 30 seconds);
no second restart is required. New empty data
stores begin on contract 1; historical team stores retain contract 2 through the
initial coordinated migration. Inspect status for blockers and check ordinary
operations throughout the rollout.

The upgrade RPC status is local to the addressed member; it reports its node ID,
reader capabilities, active contract, epoch, preparation index and known leader.
A blocked preparation/finalization identifies the first member that is not ready.
Activation changes storage interpretation, not ownership or tool permissions.


Operator mutations are audited with the verified operator and policy grant;
automatic mutations use the cluster identity and reviewed transition authority.
Configure local audit as well as Raft audit to retain completion evidence after
leadership transfer. If recording admission fails, the action is refused. If an
outcome record fails after submission, the node warns; check upgrade status before
retrying, because the action may already have committed. Reuse the exact ID,
epoch and target for a lost-response retry; changing the target is refused.

Joining-member probes have a bounded timeout and do not hold the write-admission
lease during the network call. Cancelled or expired requests waiting for admission
return without submitting. A timeout after submission still has an uncertain
outcome: inspect status/configuration rather than assuming it did not happen.
