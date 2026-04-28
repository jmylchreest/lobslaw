// Package netfilter installs nftables rules in a subprocess's
// network namespace. Belt-and-braces enforcement on top of the
// HTTPS_PROXY env var (Phase E.4): when a skill subprocess runs in
// its own netns AND has a network-filter policy, this package's
// rules drop every outbound packet that isn't loopback. Smokescreen
// reaches the subprocess via Unix-domain socket bind-mounted into
// the subprocess's mount namespace, so loopback-only egress is
// sufficient for legitimate proxy traffic.
//
// Why nftables on top of HTTPS_PROXY:
//   - HTTPS_PROXY is application-layer; honest skills honour it,
//     malicious or buggy ones can ignore it and connect direct.
//   - nftables enforces at kernel level; bypass requires
//     CAP_NET_ADMIN, which the subprocess doesn't have.
//
// Why netns isolation alone isn't sufficient: a fresh netns has
// only loopback by default — without veth wiring there's no way
// out. So the netns alone DOES drop egress. nftables here is for
// the case where operators wire veth (e.g. for DNS) but still want
// to deny direct egress beyond what the proxy offers.
//
// Application timing (deferred): the rules need to be installed
// AFTER unshare(CLONE_NEWNET) but BEFORE exec(2). Go's standard
// library doesn't expose that hook directly, so production use
// requires either a self-rexec wrapper that applies the rules then
// execs into the real handler, or a small static helper binary
// invoked as the first link of argv. The Apply* functions in this
// package handle the in-namespace work — wiring the post-clone-
// pre-exec hook is a follow-up integration step.
//
// This package is deliberately isolated from internal/sandbox so
// non-Linux builds compile cleanly and the netfilter dependency
// (nft binary) is optional.
package netfilter
