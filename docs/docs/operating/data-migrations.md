# Data upgrades and recovery

Lobslaw versions its state database, Raft command envelopes and portable archives
separately. Supported old data is validated and migrated; unknown newer formats
are refused. Do not downgrade an upgraded data directory. Keep its original backup
and matching binary for rollback.

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
