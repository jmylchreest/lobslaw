package main

import (
	"fmt"
	"io"
)

// Writing to a stopped node's state.db is not the same as writing
// through the node.
//
// The node rebuilds its state from the raft log on boot, so a direct
// write survives only if the log holds nothing later about the same
// record: a PUT is overwritten by whatever the log replays, and a
// DELETE is resurrected by it. The asymmetry is the trap — the write
// appears to have worked, and unwinds itself at the next start.
//
// This has been learned twice at the cost of a wrong conclusion each
// time (see the lobslaw-durable-writes-go-through-raft decision), and
// both times the tool reported success. Every offline form that
// mutates now says so before it does, because the live forms exist
// now: choosing the offline one is a decision, and a decision needs
// its consequence on the page.

// warnOfflineWrite prints the durability caveat for a direct write.
//
// Printed before the write rather than after, so somebody reading the
// output as it scrolls sees the warning above the thing it is about.
// Not a refusal: a cluster that will not start is exactly when this is
// the only way in, and refusing would remove the recourse it exists to
// provide.
func warnOfflineWrite(w io.Writer, subcommand string) {
	_, _ = fmt.Fprintf(w, "WARNING: %s is writing directly to state.db.\n", subcommand)
	_, _ = fmt.Fprintln(w,
		"  The node rebuilds from the raft log on boot, so this write is undone if the")
	_, _ = fmt.Fprintln(w,
		"  log still holds a later entry for the same record — a PUT is overwritten and")
	_, _ = fmt.Fprintln(w,
		"  a DELETE comes back. Durable only while the node stays down or the entry has")
	_, _ = fmt.Fprintln(w,
		"  been compacted away. Run the same command against a running node to write")
	_, _ = fmt.Fprintln(w, "  through raft instead.")
	_, _ = fmt.Fprintln(w)
}
