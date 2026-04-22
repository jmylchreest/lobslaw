// Package mtls provides mutual-TLS primitives for lobslaw's cluster
// gRPC transport: CA generation, per-node certificate signing, and
// gRPC TransportCredentials construction.
//
// The main lobslaw binary only reads the CA public cert plus this
// node's cert+key — it never touches the CA private key. The
// `lobslaw cluster ca-init` and `lobslaw cluster sign-node`
// subcommands are the only paths that consume the CA key.
package mtls
