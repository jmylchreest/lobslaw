---
sidebar_position: 9
---

# Host Binary Requirements

Some skills (`gog`, `gh`, `kubectl`-wrapping skills, etc.) need a CLI binary on the host. Lobslaw doesn't expose a generic "install any binary" tool to the agent — that'd be too coarse a power. Instead, host binaries are installed **as a side-effect of installing a skill that declares them**.

## How it works

A clawhub-format skill bundle declares its host binary requirements in its front-matter:

```yaml
---
name: gog
metadata:
  clawdbot:
    requires:
      bins: [gog]
    install:
      - id: brew
        kind: brew
        formula: steipete/tap/gogcli
        bins: [gog]
      - id: apt
        kind: apt
        package: gogcli
        bins: [gog]
        sudo: true
---
```

When the operator runs `clawhub_install steipete/gog`, the install pipeline:

1. Verifies the bundle (existing ed25519 sig check).
2. For each `requires.bins` entry: checks PATH first (baked image, bind-mount, prior install — any of these short-circuit the install).
3. If missing: picks the matching `install[]` spec for the host (linux/darwin, distro, arch) and runs it via the corresponding manager.
4. Materializes the synthetic skill registration so the agent can call `gog`.

No operator-side `[[binary]]` config. The bundle is the trust gate; granting `clawhub_install` for a slug grants whatever the bundle declares as host requirements.

## Manager pool

| Manager | OS | Sudo? | Hosts (egress role: `binaries-install`) |
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

## Install prefix

User-mode managers write into `[security] binary_install_prefix` (default `/lobslaw/usr/local`):

| Manager | How prefix is honoured |
|---|---|
| `npm` | `--prefix=$prefix` |
| `cargo` | `--root=$prefix` |
| `go-install` | `GOBIN=$prefix/bin` env |
| `uvx` (uv tool install) | `UV_TOOL_BIN_DIR=$prefix/bin UV_TOOL_DIR=$prefix/uv-tools` env |
| `pipx` | `PIPX_HOME=$prefix/pipx PIPX_BIN_DIR=$prefix/bin` env |
| `curl-sh` | `LOBSLAW_INSTALL_PREFIX=$prefix` env (script honours or doesn't) |
| System managers | Ignored — they write to system paths. Only meaningful when lobslaw runs as root inside a container with a durable rootfs. |

The default `/lobslaw/usr/local` is FHS-aligned for "operator-installed locally," distinct from `/lobslaw/usr/bin` (which `uv-init` populates) and `/usr/bin` (image-baked baseline). Skill subprocess `PATH` becomes `/lobslaw/usr/local/bin:/lobslaw/usr/bin:/usr/bin` so newer installs win precedence.

## Sudo

`sudo: true` requires **passwordless** sudo to be pre-configured on the host. The runtime probes with `sudo -n true` first; if that fails the install errors out with:

```
install requires sudo but lobslaw is not root and passwordless sudo is not configured
```

User-mode managers (`brew`, `npm`, `cargo`, `go-install`, `uvx`, `pipx`) reject `sudo: true` at validation time — that combination is almost always a typo, and brew explicitly refuses to run as root. `curl-sh` is the exception (the script may genuinely need root) and accepts sudo opt-in.

Inside Docker the lobslaw process is typically root within the container, so sudo is a no-op there.

## Rootless guidance

For "no host sudo, ever":

- Bundles that ship only user-mode install methods (npm/cargo/go-install/uvx/pipx/curl-sh) work directly.
- For bundles with system-mode methods, either run inside a container (where root-within-container is fine) or pre-install the binary via your normal package manager — the install pipeline detects it on PATH and skips.

The validator catches an explicit footgun: if a bundle declares `sudo: true` on a user-mode manager, the install fails fast with a "sudo:true is not meaningful for this manager" error.

## Skill `requires_binary`

For lobslaw-native skills (manifest.yaml format, not clawhub format), the manifest can declare:

```yaml
requires_binary:
  - gh
  - jq
```

The invoker pre-spawns: `LookPath` against `$prefix/bin:$PATH` for each entry. If any are missing, refuses to spawn with a structured error pointing at the install path.

## Reference

- `internal/binaries/` — Satisfier + Manager interface + per-manager implementations
- `internal/clawhub/` — bundle format + install pipeline
- `pkg/config/config.go` — `SecurityConfig.BinaryInstallPrefix`
- `internal/skills/skill.go` — `Manifest.RequiresBinary`
- `internal/skills/invoker.go` — pre-spawn check + PATH injection
