//go:build !debug

package node

import "context"

// startPprof is a no-op in non-debug builds. The debug variant in
// pprof_debug.go takes over when the binary is built with
// `go build -tags debug ./...`. Keeping this as a separate file +
// build constraint means pprof's transitive imports
// (net/http/pprof) only land in the binary when explicitly opted in.
func (n *Node) startPprof(_ context.Context) {}
