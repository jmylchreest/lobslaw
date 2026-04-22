// Package node holds the orchestrator that wires every cluster
// subsystem together — mTLS, gRPC server with the interceptor stack,
// Raft + bbolt (when memory/policy is enabled), discovery (registry
// + NodeService + seed-list client), and the minimal PolicyService
// shim that Phase 4 will replace.
//
// cmd/lobslaw/main.go is a thin wrapper: parse flags, load config,
// build a node.Config, call node.New + node.Start. All non-trivial
// startup logic lives here so it's testable without spawning a binary.
package node
