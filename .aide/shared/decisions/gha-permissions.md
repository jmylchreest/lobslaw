---
topic: gha-permissions
decision: Deny all at workflow level; grant minimum per-job; use OIDC over long-lived secrets
decided_by: blueprint:github-actions@0.0.59
date: 2026-04-21
references:
  - https://docs.github.com/en/actions/security-for-github-actions/security-guides/automatic-token-authentication
---

# gha-permissions

**Decision:** Deny all at workflow level; grant minimum per-job; use OIDC over long-lived secrets

## Rationale

Default GITHUB_TOKEN permissions are overly broad. Explicit per-job declarations make privilege requirements auditable. OIDC eliminates long-lived credential storage entirely.

## Details

Workflow: permissions: {}. CI jobs: contents: read. Release jobs: contents: write, id-token: write (OIDC for Sigstore/cloud auth), packages: write (if pushing to GHCR). Use OIDC with id-token: write for AWS/GCP/Azure auth instead of stored access keys.

