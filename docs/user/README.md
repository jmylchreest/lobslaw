# lobslaw — User Docs

For people installing, configuring, and running lobslaw. Developer docs live in [`../dev/`](../dev/).

## Status

User docs are stubs until the project reaches running-end-to-end state (Phase 5 ships the first useful agent loop; Phase 6 adds channels). This directory will grow as features become user-visible.

## Planned contents

| Doc | Topic |
|---|---|
| `OVERVIEW.md` | What lobslaw is, how it differs from hosted assistants, threat model |
| `INSTALL.md` | Container image, binary install, required dependencies |
| `CONFIG.md` | `config.toml` reference — every knob with defaults |
| `CLUSTER.md` | Running as a cluster — seed nodes, mTLS bootstrap, adding members |
| `SKILLS.md` | Installing and authoring skills; `policy.d/` for operator overrides |
| [`CHANNELS.md`](./CHANNELS.md) | REST / Telegram channel setup and message flow |
| `BACKUP.md` | Snapshotting, restoring, migrating between nodes |
| `TROUBLESHOOTING.md` | Common failure modes and resolutions |
| `FAQ.md` | Recurring questions |

## If you're looking for something now

- **What is lobslaw?** — [../../README.md](../../README.md) at repo root has the entry-point.
- **How do the internals work?** — [`../dev/`](../dev/) has subsystem docs.
