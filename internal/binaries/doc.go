// Package binaries satisfies host-binary requirements declared by
// installed skills. When clawhub_install lands a skill whose bundle
// declares clawdbot.requires.bins (e.g. ["gog"]) plus an install
// array (e.g. brew/apt/pacman/curl-sh methods), this package's
// Satisfier checks whether each bin is already on PATH and runs the
// matching install method when it isn't.
//
// The Satisfier is install-pipeline-internal; the agent does not call
// it directly. Operators don't write [[binary]] config — the trust
// gate is the clawhub bundle they're installing (gated separately
// via the clawhub_install policy resource).
//
// # Manager pool
//
// User-mode (no sudo): brew, pipx, uvx, npm, cargo, go-install,
// curl-sh.
// System-mode (sudo): apt, dnf, pacman, apk.
//
// curl-sh requires sha256:<64hex> on the install spec; "curl|bash"
// without a checksum is rejected.
//
// # Egress
//
// Manager subprocesses use the "binaries-install" smokescreen role.
// HostsFor() returns the union of hostnames for a set of install
// specs so the install pipeline can wire/refresh the role's
// allowlist when a skill installs.
package binaries
