package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
)

// derivedNodeID picks the node identity used by both the runtime
// (raft.ServerID) and `lobslaw cluster sign-node` (cert CN/SAN).
// Resolution: $LOBSLAW_NODE_ID > short hostname > random fallback.
// Short hostname (split at first dot) — FQDNs drift when hosts move
// DNS zones, and that drift would look to raft like a new voter.
func derivedNodeID() string {
	if v := strings.TrimSpace(os.Getenv("LOBSLAW_NODE_ID")); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil {
		short := strings.ToLower(strings.TrimSpace(strings.SplitN(h, ".", 2)[0]))
		if short != "" {
			return short
		}
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("lobslaw-%x", b)
}
