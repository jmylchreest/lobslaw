// Package clawhub is the HTTP client + install pipeline for the
// clawhub.ai skill catalog. Operators set [security.clawhub_base_url]
// to point at the catalog (defaulting to https://clawhub.ai when
// they enable it); this package handles:
//
//	1. Fetching skill metadata + the publisher's signature.
//	2. Downloading the skill bundle (tarball of manifest + handler
//	   + sidecar binary references).
//	3. Verifying the SHA-256 of the bundle against the catalog's
//	   declared digest, and the ed25519 signature against the
//	   operator's trusted publishers.
//	4. Extracting the bundle into the operator's "skill-tools"
//	   storage mount under a per-skill subpath.
//
// The skill registry's filesystem watcher then picks up the new
// manifest.yaml and registers the skill with the agent.
//
// HTTP traffic routes through internal/egress.For("clawhub") so
// the egress ACL gates lookups to the catalog host plus the
// declared binary hosts (default: github.com release endpoints).
//
// This package is the lobslaw-side client only — the clawhub.ai
// service implementation is out of scope. The wire protocol mirrors
// what the OpenClaw/clawhub-compatible registry shape implies.
package clawhub
