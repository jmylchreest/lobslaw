---
sidebar_position: 2
---

# ClawHub

ClawHub retrieval feeds the same [reviewed installation pipeline](./skill-sharing.md)
as local portable packages. Both the CLI compatibility command and the agent tool
stage skills in Raft; neither installs directly into a watched directory.

## Configure retrieval

```toml
[security]
clawhub_base_url = "https://clawhub.ai"
```

Requests use the `clawhub` egress role. The node needs Raft-backed skill storage;
a dedicated ClawHub storage mount is no longer required. Signing requirements
come from `[skills] signing_policy` and `trusted_publishers`.

The slug API retrieves `clawhub:<name>` or `clawhub:<owner>/<name>`; the owner
prefix is informational. A native catalogue can supply versioned metadata via
`clawhub:<name>@<version>`. Its advertised digest and any present catalogue
signature are checked. Retrieved files are converted into a portable artifact
with provenance; catalogue metadata does not grant sharing-signature trust.

## CLI installation

```bash
# Preview without writing.
lobslaw skills install clawhub:gog --context home --owner user:alice

# Compatibility spelling: identical options and staging behavior.
lobslaw plugin install clawhub:gog --context home --owner user:alice
```

Repeat with `--apply --expected-plan <preview-digest>` to stage the reviewed
artifact. Then use `skills activate-install <installation-id> --owner user:alice`
for a separate activation preview and apply. The full manifest, requested
permissions, binary requirements and schedules are available for review.

The former ClawHub `--root` and `--yes` options are rejected with migration
instructions. Neither is converted into activation permission. Local-directory
plugin commands retain their separate local-plugin behavior.

## Agent proposals

`clawhub_install` accepts `slug`, or `name` plus `version`. It retrieves, validates
and stages a proposal for the authenticated turn's canonical user. The tool
returns an installation ID, owner, plan and human review instructions. It has no
activation operation and does not accept an owner supplied by the model.

Two explicit policy grants are required: permission to call the tool and
permission to stage proposals for that owner. For example:

```toml
[[policy.rules]]
id = "alice-clawhub-tool"
subject = "user:alice"
action = "tool:exec"
resource = "clawhub_install"
effect = "allow"
priority = 20

[[policy.rules]]
id = "alice-clawhub-proposal"
subject = "user:alice"
action = "skills:share:propose"
resource = "user:alice"
effect = "allow"
priority = 20
```

Proposal permission does not confer activation permission. An operator reviews
and activates with the CLI using an operator certificate, the configured operator
data role and a `skills:share:activate` grant. Agent retries reuse the installation
ID. An already-active identical installation is reported as such; newly staged
content receives no approval.

## Permissions and dependencies

Retrieval and staging never download host binaries, bootstrap package managers or
create execution policy rules. Review required binaries and permissions in the
manifest; operators must arrange dependency installation and tool permissions
separately. Missing dependencies remain subject to normal runtime checks.

The old `mount`, `subpath` and `bootstrap_managers` tool arguments are rejected.
`clawhub_auto_emit_install_rules` is retained as a deprecated config field for
compatibility, but has no effect and emits a startup warning when enabled.
`clawhub_install_mount` is also deprecated and unused by the new installation flow.
Existing installed skills and policy rules are not automatically revoked.

## Implementation

- `internal/clawhub/share.go`: retrieval and portable conversion.
- `internal/node/clawhub_proposal.go`: turn ownership, proposal policy and staging.
- `internal/node/skill_sharing.go`: common validation and operator activation.
- `internal/tools/clawhub.go`: proposal-only agent interface.
- `cmd/lobslaw/plugin_clawhub.go`: CLI compatibility wrapper.
