package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Repointing a principal on a running cluster.
//
// The offline form wrote straight to bbolt, so it needed the node
// stopped — and pointed at a follower's file while the cluster ran it
// would have written ownership no other replica has. The live form
// replicates, which is the only version of this that is safe on a
// cluster with more than one node.

func identityClient(node *liveNode) (lobslawv1.IdentityServiceClient, func(), error) {
	conn, err := node.dial()
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewIdentityServiceClient(conn), func() { _ = conn.Close() }, nil
}

func identityRebindLive(args []string) error {
	fs := flag.NewFlagSet("identity rebind", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	apply := fs.Bool("apply", false, "actually rewrite (default is a dry run)")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	positional, perr := parseFlagsAndPositionals(fs, args)
	if perr != nil {
		return perr
	}
	req, err := rebindRequest(positional, *apply)
	if err != nil {
		return err
	}

	client, closeConn, err := identityClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.Rebind(ctx, req)
	if err != nil {
		return err
	}
	return renderRebind(os.Stdout, planFromResponse(res),
		req.GetFrom(), req.GetTo(), node.addr, *apply, *asJSON)
}

// rebindRequest builds the request from the positionals.
//
// DryRun is the INVERSE of --apply, which is the one bit of this
// command that must never invert: a rebind rewrites ownership across
// seven buckets and there is no undo.
func rebindRequest(args []string, apply bool) (*lobslawv1.RebindRequest, error) {
	from, to, err := rebindArgs(args)
	if err != nil {
		return nil, err
	}
	return &lobslawv1.RebindRequest{From: from, To: to, DryRun: !apply}, nil
}

// rebindArgs validates the positional pair.
//
// Checked before dialling as well as at the server, so a mistyped
// invocation costs no round trip — and so `rebind alice alice` is
// refused rather than reporting a confident zero.
func rebindArgs(args []string) (from, to string, err error) {
	if len(args) != 2 {
		return "", "", fmt.Errorf("usage: lobslaw identity rebind <from> <to> [--apply]")
	}
	from, to = strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	switch {
	case from == "" || to == "":
		return "", "", fmt.Errorf("both <from> and <to> are required")
	case from == to:
		return "", "", fmt.Errorf("<from> and <to> are the same id (%q); nothing to do", from)
	}
	return from, to, nil
}

// planFromResponse turns the wire shape back into the plan both forms
// render, so the dry run an operator reads offline is word-for-word
// the one they read live.
func planFromResponse(res *lobslawv1.RebindResponse) *memory.RebindPlan {
	plan := &memory.RebindPlan{Conflicts: res.GetConflicts()}
	for _, c := range res.GetChanges() {
		if plan.Changes == nil {
			plan.Changes = map[string][]string{}
		}
		plan.Changes[c.GetBucket()] = c.GetIds()
	}
	return plan
}

// renderRebind reports what would move, what will not, and whether it
// happened.
//
// The conflicts are printed whether or not anything moved: a
// half-moved identity is worse than one that did not move, because
// nothing says which half, and the SKIPPED lines are the only place
// that says so.
func renderRebind(w io.Writer, plan *memory.RebindPlan,
	from, to, source string, applied, asJSON bool) error {
	if asJSON {
		return emitJSON(map[string]any{
			"applied":   applied,
			"source":    source,
			"from":      from,
			"to":        to,
			"changes":   plan.Changes,
			"conflicts": plan.Conflicts,
			"total":     plan.Total(),
		})
	}

	_, _ = fmt.Fprintf(w, "%s\n", source)
	_, _ = fmt.Fprintf(w, "%s -> %s\n\n", from, to)
	for _, bucket := range plan.Buckets() {
		ids := plan.Changes[bucket]
		_, _ = fmt.Fprintf(w, "  %-20s %d record(s)\n", bucket, len(ids))
		fprintSample(w, ids)
	}
	for _, c := range plan.Conflicts {
		_, _ = fmt.Fprintf(w, "  SKIPPED: %s\n", c)
	}
	switch {
	case plan.Total() == 0:
		_, _ = fmt.Fprintln(w, "\nnothing owned by that id.")
	case applied:
		_, _ = fmt.Fprintf(w, "\nREBOUND %d record(s).\n", plan.Total())
	default:
		_, _ = fmt.Fprintf(w,
			"\nDRY RUN — nothing was written. Re-run with --apply to rebind %d record(s).\n", plan.Total())
	}
	return nil
}
