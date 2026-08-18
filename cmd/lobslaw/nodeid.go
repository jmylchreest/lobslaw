package main

import (
	"crypto/rand"
	"fmt"
	"os"
	"strings"
)

// dispatchNodeID handles `lobslaw nodeid`.
//
// It existed as a documented subcommand with no dispatcher, so it fell
// through to the main path and BOOTED THE WHOLE ASSISTANT — reading
// whatever config was on the machine, joining raft, and starting every
// channel. `--node-id $(lobslaw nodeid)` appears in the getting-started
// guide, so following the documentation started a second node.
//
// A command that silently does something enormous is worse than one
// that errors, which is why the inventory test now checks that every
// documented subcommand is claimed by a dispatcher.
func dispatchNodeID(args []string) bool {
	if findSubcmd(args, "nodeid") < 0 {
		return false
	}
	fmt.Println(derivedNodeID())
	return true
}

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
