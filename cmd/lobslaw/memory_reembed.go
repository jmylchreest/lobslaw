package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Re-embedding through the RUNNING node.
//
// The offline backfill-embeddings tool writes state.db directly, and
// the node rebuilds state from its raft log on boot — so a PUT still
// in the log re-applies and resurrects anything that tool deleted. Its
// writes survived and its deletions did not, which meant a model
// change could never fully land: one resurrected vector carrying the
// old stamp is enough for the next boot to be refused, and re-running
// the tool never fixed it.
//
// This asks the node to do the same work through raft, where it is
// durable. It also needs no memory key and no stopping: the node
// already holds the store open and the embedder loaded.

const memoryReembedUsage = `lobslaw memory reembed — rewrite every vector with the current model

  lobslaw memory reembed [--limit N]

Re-embeds every episodic record using the model this node has loaded,
replaces the superseded vectors, and removes any left stamped with a
previous model.

Run it after changing [compute.embeddings] model. The node refuses to
start when its corpus was written by a different model, and this is
what clears that.

The node must be RUNNING — the opposite of backfill-embeddings, and
the reason this exists: mutations go through raft here, so they
survive a restart.

  --limit N   process at most N records, for a trial on a large corpus
`

func memoryReembedLive(args []string) error {
	fs := flag.NewFlagSet("memory reembed", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	limit := fs.Int("limit", 0, "process at most this many records (0 = all)")
	fs.Usage = func() { _, _ = fmt.Fprint(fs.Output(), memoryReembedUsage) }
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		// Refused rather than ignored: a stray argument here is far
		// more likely a mistyped flag than something to discard, and
		// this command rewrites the whole corpus.
		fmt.Fprint(os.Stderr, memoryReembedUsage)
		return fmt.Errorf("memory reembed takes no positional arguments, got %v", positional)
	}

	client, closeConn, err := memoryClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	// Generous: re-embedding a large corpus is minutes of work, and a
	// timeout that fires halfway leaves the store half-migrated with
	// no indication of how far it got.
	if node.timeout < time.Hour {
		node.timeout = time.Hour
	}
	ctx, cancel := node.ctx()
	defer cancel()

	res, err := client.Reembed(ctx, &lobslawv1.ReembedRequest{
		Limit: int32(*limit), //nolint:gosec // a CLI --limit is not attacker-controlled
	})
	if err != nil {
		return explainUnimplemented(err, "memory reembed")
	}

	fmt.Printf("model:       %s\n", res.GetModel())
	fmt.Printf("re-embedded: %d record(s)\n", res.GetReembedded())
	fmt.Printf("replaced:    %d superseded vector(s)\n", res.GetReplaced())
	if res.GetOrphans() > 0 {
		fmt.Printf("orphans:     %d vector(s) removed (stamped with a previous model)\n", res.GetOrphans())
	}
	if res.GetUnreadable() > 0 {
		// Worth saying out loud: those records will never be recalled,
		// and nothing else in the system will mention them.
		fmt.Printf("unreadable:  %d record(s) skipped — written under a different memory key\n",
			res.GetUnreadable())
	}
	return nil
}
