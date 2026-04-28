---
sidebar_position: 9
---

# Binary Install

Operator-declared OS-binary install. Skills sometimes need a binary on the host that isn't part of the bundle — `gh`, `gcloud`, `uvx`, `ffmpeg`, etc. Instead of letting the agent run arbitrary `apt install`, lobslaw uses a constrained registry: the operator declares which binaries are allowed and how to install them; the agent picks a name and the runtime picks a manager.

## Why a registry

Three properties that matter:

1. **Trust boundary.** The agent never invents install commands. It picks names from a catalogue you've declared. Anything not in the catalogue is `unknown binary`.
2. **Per-OS install matrix.** One `[[binary]]` declares apt for Debian, pacman for Arch, brew for macOS — the runtime picks the right one for the host.
3. **Idempotent.** A `detect` command short-circuits when the binary is already there. Re-running `binary_install gh` on a host that has `gh` is a no-op.

## Configuration

```toml
[[binary]]
name        = "gh"
description = "GitHub CLI"
detect      = "gh --version"

[[binary.install]]
os      = "linux"
distro  = "debian"            # narrow further; matches debian + ubuntu
manager = "apt"
package = "gh"
sudo    = true

[[binary.install]]
os      = "linux"
distro  = "arch"
manager = "pacman"
package = "github-cli"
sudo    = true

[[binary.install]]
os      = "darwin"
manager = "brew"
package = "gh"
```

```toml
[[binary]]
name        = "uvx"
description = "Python tool runner"
detect      = "uvx --version"

[[binary.install]]
os       = "linux"
manager  = "curl-sh"
url      = "https://astral.sh/uv/install.sh"
checksum = "sha256:abc123..."   # required — curl|bash without checksum is rejected
```

## Schema

Each `[[binary]]`:

| Field | Required | Type |
|---|---|---|
| `name` | yes | string — alphanumeric + dash + dot + underscore |
| `description` | no | string — surfaces in `binary_list` |
| `detect` | no | string — shell command, exit 0 = installed |
| `install` | yes | array of install specs |

Each `[[binary.install]]`:

| Field | Required | Type |
|---|---|---|
| `os` | yes | "linux" / "darwin" / "windows" |
| `arch` | no | "amd64" / "arm64" — empty matches any |
| `distro` | no | linux only — "debian" / "arch" / "fedora" / "alpine" — uses /etc/os-release ID + ID_LIKE |
| `manager` | yes | apt / brew / pacman / dnf / apk / pipx / uvx / npm / cargo / go-install / curl-sh |
| `package` | yes (except curl-sh) | manager-specific package name |
| `repo` | no | apt repo line for apt-get; informational for others |
| `url` | yes for curl-sh | install script URL |
| `checksum` | yes for curl-sh | `sha256:<64-hex>` of the script body |
| `sudo` | no, default false | whether the install command needs sudo |
| `args` | no | extra args appended to the manager command |

## Managers

| Manager | OS | Sudo? | Hosts |
|---|---|---|---|
| `apt` | linux (debian/ubuntu) | yes | deb.debian.org, archive.ubuntu.com, … |
| `brew` | darwin | **no** (refuses sudo) | formulae.brew.sh, github.com, ghcr.io |
| `pacman` | linux (arch) | yes | mirror.archlinux.org, geo.mirror.pkgbuild.com |
| `dnf` | linux (fedora/rhel) | yes | mirrors.fedoraproject.org, … |
| `apk` | linux (alpine) | yes | dl-cdn.alpinelinux.org |
| `pipx` | any | no | pypi.org, files.pythonhosted.org |
| `uvx` | any | no | pypi.org (runs `uv tool install`) |
| `npm` | any | no | registry.npmjs.org |
| `cargo` | any | no | crates.io |
| `go-install` | any | no | proxy.golang.org |
| `curl-sh` | any (POSIX) | optional | the script's URL host (per-spec) |

The `curl-sh` manager fetches a script via the egress proxy, verifies its SHA-256 against `checksum`, then executes it via `/bin/sh`. **No checksum, no install** — the validator rejects curl-sh specs without a sha256.

## Sudo

`sudo: true` requires **passwordless** sudo to be pre-configured on the host. The runtime probes with `sudo -n true` first; if that prompts (or fails for any reason) the install errors out with:

```
install requires sudo but lobslaw is not root and passwordless sudo is not configured
```

Inside Docker, the lobslaw process is typically root — sudo is a no-op. Outside Docker, the operator either:

- Runs lobslaw under a user with `NOPASSWD` for the specific manager binaries, or
- Pre-installs the binaries through normal channels and lets the registry's `detect` short-circuit.

The runtime never tries to elevate beyond `sudo -n`. There is no "ask for a password" path.

## Egress

Each manager declares its upstream hostnames. The union seeds the smokescreen `binaries-install` egress role at boot. The install subprocess uses `HTTPS_PROXY=...?role=binaries-install`; smokescreen tunnels only to declared hosts.

You can verify the role's allowlist with:

```bash
lobslaw doctor --check egress
```

## Policy

A single resource `binary_install` opens the whole declared catalogue. Operators don't write per-binary policy — the catalogue is the trust gate. By default, neither `binary_install` nor `binary_list` is allowed; add:

```toml
[[policy.rules]]
id       = "owner-binary-tools"
priority = 20
effect   = "allow"
subject  = "scope:owner"
action   = "tool:exec"
resource = "binary_*"
```

If you want only `binary_list` (so the agent can introspect but not install), drop the `_*` glob and rule the install path explicitly.

## Install prefix and storage layout

The container layout has three tiers, each owned by a different actor:

```
/usr/bin/                  ← image-baked baseline (busybox)
                              owner: the lobslaw image
                              mode: read-only

/lobslaw/usr/bin/          ← bootstrap tools (uv, uvx)
                              owner: the uv-init container
                              populated at boot from a sibling image
                              mode: read-only at runtime

/lobslaw/usr/local/bin/    ← operator-installed binaries (gh, jq, ...)
                              owner: the lobslaw process via binary_install
                              mode: rw for lobslaw, rx for skills
```

Skill subprocess `PATH` becomes `/lobslaw/usr/local/bin:/lobslaw/usr/bin:/usr/bin` — agent-installed binaries take precedence; uv/uvx fall through; busybox last.

Configure:

```toml
[security]
binary_install_prefix = "/lobslaw/usr/local"

[[storage.mounts]]
label = "binaries"
type  = "local"
path  = "/lobslaw/usr/local"
mode  = "rx"           # skill subprocesses get read+exec, no write
```

The lobslaw process has implicit rw on the prefix (it's the installer). Skill subprocesses see it as an `rx` mount — they can execute installed binaries but can't tamper with them.

Why a separate path from `tools-runtime`:

- **Ownership clarity.** `tools-runtime` (`/lobslaw/usr/bin`) is populated by the `uv-init` sidecar; lobslaw shouldn't write there. `binaries-workspace` (`/lobslaw/usr/local`) is lobslaw's own territory.
- **No collisions.** A `uv-init` rerun at restart could clobber whatever `binary_install` had left in `tools-runtime`.
- **FHS convention.** `/usr/local` is the conventional path for "operator-installed locally"; following it means a future operator who's never read these docs can guess what's there.

## Persistence vs. ephemeral

Pure deploy choice — the registry doesn't care:

- **Persistent**: mount a named volume (or host bind-mount) at the prefix path. Installs survive container restarts; share the same volume across cluster nodes for one-install-many-views.
- **Ephemeral**: don't mount anything at the prefix path. Installs live on the container's writable layer; reset on every container restart. Fine for fast managers (`uvx`, `pipx`, most `go install`); painful for slow ones (`cargo install` is minutes per crate).

The reference `deploy/docker/cluster.yml` declares `binaries-workspace` as a named volume mounted at `/lobslaw/usr/local`. Drop the entry from `volumes:` to make it ephemeral.

## Manager prefix support

| Manager | How prefix is honoured |
|---|---|
| `npm` | `--prefix=$prefix` |
| `cargo` | `--root=$prefix` |
| `go-install` | `GOBIN=$prefix/bin` env |
| `uvx` (uv tool install) | `UV_TOOL_BIN_DIR=$prefix/bin UV_TOOL_DIR=$prefix/uv-tools` env |
| `pipx` | `PIPX_HOME=$prefix/pipx PIPX_BIN_DIR=$prefix/bin` env |
| `curl-sh` | `LOBSLAW_INSTALL_PREFIX=$prefix` env (the script chooses to honour or not) |
| `apt`/`dnf`/`pacman`/`apk` | **Not honoured.** System managers write to system paths; only valid in ephemeral container deployments where the whole image gets reset. |
| `brew` | Brew has its own prefix model; left for separate work. |

## Rootless guidance

If your goal is "no host sudo, ever":

- Use only **user-mode managers** (npm, cargo, go-install, uvx, pipx, curl-sh-with-checksum).
- Set `binary_install_prefix` to a path you can write to as the lobslaw user.
- Don't add `[[binary]]` entries with `apt`/`dnf`/`pacman`/`apk` — they require system-level access and the validator will reject `sudo: true` on user-mode managers.

If you're inside a container where lobslaw is root:

- Use any manager. apt/dnf/pacman/apk install to system paths inside the container; bind-mount `/lobslaw/usr` from a host volume so the bits survive container restarts.

The validator catches an explicit footgun: `sudo: true` combined with a user-mode manager (npm, cargo, go-install, etc.) errors at boot — that combination is almost always a typo, not an intentional choice.

## Skill manifest integration

A skill manifest declares `requires_binary` for any host binaries the skill needs at exec time:

```yaml
requires_binary:
  - gh
  - jq
```

The invoker:

1. Pre-spawn, runs `LookPath(name)` against `$prefix/bin:$PATH` for each entry.
2. If any are missing, refuses to spawn with: `requires_binary "gh" not installed — try `binary_install gh` (operator must declare it in [[binary]] first)`.
3. If all are present, injects `PATH=$prefix/bin:$PATH` into the subprocess's env so the skill's `gh ...` invocations resolve.

The intended UX: user asks the agent to do something, the agent calls a skill, the skill says "I need gh", the agent installs gh (if policy allows), the skill retries and works. No human in the loop unless `binary_install` is gated `require_confirmation`.

## What's not here

- **Auto-discovery of installed packages.** lobslaw doesn't enumerate `dpkg -l` to figure out what's already installed beyond the operator's `detect` command. The detect command IS the source of truth.
- **Auto-update.** No equivalent of `binary_update gh`. The operator handles upgrades through their normal package manager workflow.
- **Removal.** No `binary_uninstall`. Operators remove binaries through normal channels; lobslaw doesn't track install state in raft.

These are deliberate omissions — they expand the trust surface without adding much value for the agent's actual use cases.

## Reference

- `internal/binaries/` — registry, managers, distro detection
- `internal/compute/builtin_binaries.go` — `binary_install` + `binary_list`
- `internal/node/wire_subsystems.go` — `wireBinaries` stage
- `internal/egress/builder.go` — `binaries-install` role construction
