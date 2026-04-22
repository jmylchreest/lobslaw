---
topic: go-release-workflow
decision: Use matrix builds with go build and gh release create for releases; inject version via ldflags; generate checksums
decided_by: blueprint:go-github-actions@0.0.59
date: 2026-04-21
---

# go-release-workflow

**Decision:** Use matrix builds with go build and gh release create for releases; inject version via ldflags; generate checksums

## Rationale

A matrix build with go build gives full control over the build process, supports CGO cross-compilation with zig, and avoids third-party tool dependencies. ldflags version injection is the standard Go pattern. gh release create is built into GitHub's own CLI.

## Details

release.yml (tag v*). Permissions: contents: write, id-token: write (OIDC optional). Checkout fetch-depth: 0, setup-go, matrix build, SHA256 checksums, gh release create.
ldflags: -s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}
Add workflow_dispatch for re-runs. GoReleaser viable for pure-Go without CGO.

