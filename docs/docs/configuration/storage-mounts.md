---
sidebar_position: 6
---

# Storage Mounts

How filesystem paths are exposed to subprocess sandboxes.

## The model

```
host filesystem
  │
  ├── /workspace                    ← mounted as label "workspace"
  ├── /var/lib/lobslaw/skills       ← mounted as label "skill-tools"
  └── ...
```

Each `[[storage.mounts]]` block declares a name (label), an underlying host path, and a mode. Subprocesses see only the mounts the operator declared, scoped further by what each subprocess's policy allows.

## Schema

```toml
[storage]
default_mount = "workspace"

[[storage.mounts]]
label    = "workspace"
type     = "local"
path     = "/workspace"
mode     = "rw"

[[storage.mounts]]
label    = "skill-tools"
type     = "local"
path     = "/var/lib/lobslaw/skills"
mode     = "ro"
```

| Field | Notes |
|---|---|
| `label` | unique mount name; subprocesses reference this, not the host path |
| `type` | `local` only today; `s3`, `nfs` etc. on the roadmap |
| `path` | absolute host path |
| `mode` | `rw`, `ro`, or `rwx` |

## Default mounts

Two are conventional:

| Label | Purpose |
|---|---|
| `workspace` | The operator's working files. Skills with declared workspace access see this; the agent's own writes go here. |
| `skill-tools` | Where clawhub installs skill bundles. Mounted `ro` for skills (they read their own manifest + binaries from here). |

## Mounting in skill manifests

A skill manifest declares which mounts it needs:

```yaml
mounts:
  - mount: workspace
    subpath: shared/calendars        # optional — narrows to a subdirectory
    mode: ro                         # narrows the mount's mode for this skill
  - mount: skill-tools
    mode: ro
```

The invoker resolves `mount: workspace` + `subpath: shared/calendars` to the host path `/workspace/shared/calendars` and adds it to the subprocess's Landlock allowlist with mode ≤ what the mount declares.

A skill can never widen — if the mount is `ro` the subprocess can't get `rw` even if it asks.

## Subpath isolation

Subpath mounts let multiple skills share a workspace without seeing each other:

```yaml
mounts:
  - mount: workspace
    subpath: gws-workspace/cache
    mode: rwx
```

The skill's view: `/workspace/gws-workspace/cache` is its private R/W area; everything else under `/workspace` is invisible.

## Custom mounts

Add as many as you want:

```toml
[[storage.mounts]]
label = "media"
type  = "local"
path  = "/srv/media"
mode  = "ro"
```

Skills opt in by referencing the label.

## Sandbox interaction

Mount declarations are *capabilities* — they make a path *available* to skills. The actual filesystem visibility is enforced by Landlock at spawn time:

1. Subprocess clone with `CLONE_NEWNS` (mount namespace).
2. The reexec helper builds a Landlock ruleset: `ALLOW read on declared ro mounts`, `ALLOW read+write on declared rw mounts`, deny everything else.
3. Helper `landlock_restrict_self()`, then `execve` the real tool.

A subprocess that tries to read `/etc/passwd` gets `EACCES` from the kernel. Same for writes outside its declared mounts.

See [Sandbox](/security/sandbox) for the full layering.

## Reference

- `pkg/config/config.go` — `StorageConfig`, `MountConfig`
- `internal/compute/mount_resolver.go` — resolve label+subpath → host path
- `internal/sandbox/install_linux.go` — Landlock ruleset builder
- `internal/skills/skill.go` — manifest parsing for mount declarations
