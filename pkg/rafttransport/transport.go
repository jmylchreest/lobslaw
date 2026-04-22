package rafttransport

import (
	"fmt"

	jillet "github.com/Jille/raft-grpc-transport"
	"github.com/hashicorp/raft"
	"google.golang.org/grpc"
)

// Config configures the gRPC-backed Raft transport.
type Config struct {
	// LocalAddr is this node's address as it appears in the Raft
	// configuration (host:port). Peers dial this to reach us.
	LocalAddr raft.ServerAddress
	// DialOpts are applied to outbound gRPC connections to peers.
	// Must include transport credentials (mTLS) for production use.
	DialOpts []grpc.DialOption
}

// Transport wraps github.com/Jille/raft-grpc-transport to provide a
// lobslaw-shaped API. Internally it delegates the wire format and
// connection pooling to that library, which uses its own compiled
// proto definitions — we don't have to own them.
//
// If audit attribution later requires reading the gRPC peer's TLS
// cert SAN inside the server-side RPC handlers, we can fork the
// transport; until then, verbatim is enough.
type Transport struct {
	mgr *jillet.Manager
}

// New constructs a Transport. Call Register on it to mount the
// gRPC handlers on a shared grpc.Server, and RaftTransport to get
// the raft.Transport implementation for NewRaft.
func New(cfg Config) (*Transport, error) {
	if cfg.LocalAddr == "" {
		return nil, fmt.Errorf("Config.LocalAddr is required")
	}
	mgr := jillet.New(cfg.LocalAddr, cfg.DialOpts)
	return &Transport{mgr: mgr}, nil
}

// RaftTransport returns the raft.Transport implementation to pass
// into hashicorp/raft's NewRaft.
func (t *Transport) RaftTransport() raft.Transport {
	return t.mgr.Transport()
}

// Register mounts the raft gRPC service on s so that peers can reach
// this node's raft endpoint over the same server that hosts every
// other cluster service.
func (t *Transport) Register(s *grpc.Server) {
	t.mgr.Register(s)
}
