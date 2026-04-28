// Package binaries implements operator-declared OS-binary install for
// the agent. Skills sometimes need a binary on the host that isn't
// part of the bundle — gh, gcloud, uvx, ffmpeg, etc. The agent calls
// binary_install(name); a Registry built from [[binaries]] in
// config.toml decides whether the binary is allowed and how to
// install it on the running OS.
//
// # Trust model
//
// The agent never invents install commands. It picks a name from the
// operator-declared registry; the registry knows how to install that
// specific binary using a constrained set of managers (apt, brew,
// pacman, dnf, curl-sh-with-checksum, etc.). The operator declares
// the binaries they trust and the supply chain they want.
//
// Defaults:
//   - All binary_install calls go through the policy engine
//     (resource = "binary_install:<name>"). Default-deny; the
//     operator opens specific binaries per scope.
//   - The install subprocess uses the "binaries-install" smokescreen
//     egress role, with hosts derived from the manager (apt repos,
//     brew CDN, pypi, etc.).
//   - sudo is opt-in per-binary and requires passwordless sudo to be
//     pre-configured on the host. Inside Docker the binary registry
//     assumes root; outside Docker, sudo refusal is an explicit error.
//
// # What this is not
//
// This is NOT clawhub's bundle-binary pipeline (internal/clawhub/
// binaries.go). That handles binaries shipped INSIDE a clawhub
// bundle (URL + SHA256, downloaded into the skill's bin/). This
// package handles binaries the operator wants the OS package
// manager (or equivalent) to install at the system level.
package binaries
