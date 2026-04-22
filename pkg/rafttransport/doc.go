// Package rafttransport implements hashicorp/raft's Transport interface
// over gRPC, riding the cluster's existing mTLS gRPC connection pool.
// Seeded from github.com/Jille/raft-grpc-transport; customised to
// carry peer identity from the mTLS cert SAN into request context for
// audit attribution.
package rafttransport
