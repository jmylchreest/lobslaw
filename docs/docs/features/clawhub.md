---
sidebar_position: 2
---

# ClawHub

[ClawHub](https://clawhub.ai) is a community skills hub — discover, install, and verify skills.

It's the recommended distribution channel because:

- **Signed bundles.** Maintainers sign their releases with ed25519; the install pipeline verifies before extraction.
- **Versioned.** `clawhub:gws-workspace@1.0.0` pins exactly. Upgrades are explicit.
- **Sandboxed by default.** Bundles ship with a manifest declaring mounts, networks, credentials — the operator can review before installing.

## Enable it

```toml
[security]
clawhub_base_url       = "https://clawhub.ai"
clawhub_install_mount  = "skill-tools"
clawhub_signing_policy = "prefer"      # off | prefer | require
```

Setting `clawhub_base_url` does two things:

1. Adds `clawhub.ai` to the smokescreen `clawhub` egress role.
2. Enables the `clawhub_install` builtin (still policy-gated separately).

Then add a policy rule so the operator can call it:

```toml
[[policy.rules]]
id       = "owner-clawhub-install"
priority = 20
effect   = "allow"
subject  = "scope:owner"
action   = "tool:exec"
resource = "clawhub_install"
```

## Signing policy

```toml
clawhub_signing_policy = "off"        # accept unsigned (DEV ONLY)
clawhub_signing_policy = "prefer"     # accept unsigned but warn
clawhub_signing_policy = "require"    # reject unsigned bundles
```

The signature scheme:

- ed25519 signature of `SHA-256(bundle.tar.gz)`.
- Trust anchored by clawhub's published platform key.
- A bundle ships `manifest.sig` + `manifest.pub`. The platform key validates the manifest publisher's pubkey; the publisher's key validates the bundle.

This is the standard delegated-trust model — clawhub signs the publisher, the publisher signs the bundle. Operators don't manage individual publisher keys.

## Install via CLI

```bash
lobslaw plugin install clawhub:gws-workspace@1.0.0
```

What happens:

1. Resolves `gws-workspace@1.0.0` → bundle URL on clawhub.
2. Fetches via the `clawhub` egress role (so `clawhub.ai` must be in the ACL).
3. Verifies SHA-256 + ed25519 per signing policy.
4. Extracts to `skill-tools` mount with tar-slip defence (`guardEntryPath` + escape-prefix check).
5. `chmod +x` on declared binaries.
6. Watcher picks up the new manifest.yaml; the registry registers each tool.

## Install via the agent

If `clawhub_install` is policy-allowed for your scope:

> install gws-workspace from clawhub

The agent calls `clawhub_install(bundle="gws-workspace@1.0.0")` directly. Output:

> Installed gws-workspace 1.0.0. 6 new tools available: gmail.search, gmail.send, calendar.list_events, calendar.create_event, drive.list, drive.read.
>
> Note: skills require explicit allow rules. To enable, add:
>
>     [[policy.rules]]
>     id       = "owner-can-call-gws-workspace"
>     priority = 20
>     effect   = "allow"
>     subject  = "scope:owner"
>     action   = "tool:exec"
>     resource = "gws-workspace.*"

## Binaries

Skills sometimes need binaries that aren't in the bundle (e.g. a skill that wraps `gh` needs `gh` installed on the host). ClawHub's binary manifest declares them:

```yaml
binaries:
  - name: gh
    versions:
      - os: linux
        arch: amd64
        url: https://github.com/cli/cli/releases/download/v2.50.0/gh_2.50.0_linux_amd64.tar.gz
        sha256: abc123...
```

The install pipeline downloads matching binaries to the skill's `bin/` and `chmod +x`. ClawHub binary URLs are matched against `[security] clawhub_binary_hosts` (default: github.com release hosts) — operators with stricter supply-chain requirements declare their own.

A more general OS-package binary install path (`apt`, `brew`, `pacman`, `pipx`) is on the roadmap — see [Roadmap → Binary Registry](#).

## Authoring a bundle

For skill authors:

1. Write the manifest + binary.
2. Sign the manifest with your publisher key.
3. Submit to clawhub.

Full author docs: [clawhub.ai/docs/publishing](https://clawhub.ai).

## Self-hosting

You don't need clawhub. Skills can be installed manually (drop into `skill-tools`), or you can run a self-hosted clawhub-compatible registry by setting:

```toml
[security]
clawhub_base_url = "https://your-internal-hub.example.com"
```

The same install pipeline works as long as the API surface matches.

## Reference

- `internal/clawhub/` — fetch, signing, install pipeline
- `internal/compute/builtin_clawhub.go` — agent-callable installer
- `cmd/lobslaw/plugin_clawhub.go` — CLI install
