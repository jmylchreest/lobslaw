---
topic: lobslaw-skill-trust
decision: "Skill trust model is clawhub-compatible by default: trust-on-install-after-operator-review. Install flow displays manifest + source tree; operator approves; lobslaw records a SHA-256 of the manifest+handler tree. Registry changes trigger re-approval. Optional hardening via skills.require_signed=true enables minisign verification against a local trusted_publishers.toml. Skills declaring sidecar:true or wildcard security.fs/network always trigger explicit operator confirmation regardless of signing status"
date: 2026-04-22
---

# lobslaw-skill-trust

**Decision:** Skill trust model is clawhub-compatible by default: trust-on-install-after-operator-review. Install flow displays manifest + source tree; operator approves; lobslaw records a SHA-256 of the manifest+handler tree. Registry changes trigger re-approval. Optional hardening via skills.require_signed=true enables minisign verification against a local trusted_publishers.toml. Skills declaring sidecar:true or wildcard security.fs/network always trigger explicit operator confirmation regardless of signing status

## Rationale

ClawHub itself does not sign skills - its trust is social (GitHub-age + reporting + moderation). Matching that keeps clawhub sync working. Adding a SHA pin per-approval gives trust-on-first-use semantics for a defender-in-depth layer. Optional minisign for the paranoid without forcing ceremony on casual users

