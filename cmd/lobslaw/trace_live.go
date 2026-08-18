package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/jmylchreest/lobslaw/internal/trace"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Asking a SPECIFIC node what it recorded.
//
// Traces are per-node files, deliberately: R24 kept them out of raft so
// a trace never costs a replicated write. That decision stands, and
// this does not undo it. What it fixes is the CLI reading whatever
// directory happens to be on the machine it runs on — which on an
// operator's laptop is either nothing, or worse, a stale copy reported
// as the cluster's.

func traceClient(node *liveNode) (lobslawv1.TraceServiceClient, func(), error) {
	conn, err := node.dial()
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewTraceServiceClient(conn), func() { _ = conn.Close() }, nil
}

// traceSource labels an answer with the node that gave it.
//
// A node id alone would be ambiguous on a cluster where two nodes were
// misconfigured with the same one, so the address it was reached at
// goes alongside.
func traceSource(nodeID, addr string) string {
	if nodeID == "" {
		return "node at " + addr
	}
	return fmt.Sprintf("node %s (%s)", nodeID, addr)
}

// tracingOffNote is what a node with tracing disabled gets told.
//
// Distinguished from "served no turns" because they look identical and
// only one of them is fixed by editing config. Getting this wrong
// sends somebody hunting for a turn that was never recorded.
const tracingOffNote = "tracing is OFF on this node — set [trace] enabled = true and restart"

func traceListLive(args []string) error {
	fs := flag.NewFlagSet("trace list", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	limit := fs.Int("limit", 20, "how many turns to list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, closeConn, err := traceClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.ListTurns(ctx, &lobslawv1.ListTurnsRequest{
		Limit: int32(*limit), //nolint:gosec // a CLI --limit is not attacker-controlled
	})
	if err != nil {
		return err
	}

	source := traceSource(res.GetNodeId(), node.addr)
	fmt.Println(source)
	if !res.GetEnabled() {
		fmt.Println(tracingOffNote)
		return nil
	}
	if len(res.GetTurnIds()) == 0 {
		fmt.Println("no turns recorded on this node")
		return nil
	}
	for _, id := range res.GetTurnIds() {
		fmt.Println(id)
	}
	return nil
}

// traceTurnID parses fs and returns the single positional turn id.
//
// Split out from traceShowLive so the parse can be tested without a
// node to dial: the bug it fixes was invisible in every error message.
// Reading the turn id from args[0] BEFORE parsing flags meant
// `trace --context prod <id>` took "--context" as the turn id, then
// failed to connect for unrelated reasons — so the wrong-id part
// never showed up, and the eventual "no spans recorded for that turn"
// read as "it ran on another node".
func traceTurnID(fs *flag.FlagSet, args []string) (string, error) {
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return "", err
	}
	switch len(positional) {
	case 1:
		return positional[0], nil
	case 0:
		return "", errors.New("a turn id is required: lobslaw trace <turn-id>")
	default:
		// Two ids is a typo, not a request for both. Quietly using the
		// first would answer about a turn nobody asked about.
		return "", fmt.Errorf("exactly one turn id is required, got %d: %v", len(positional), positional)
	}
}

func traceShowLive(args []string) error {
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	turnID, err := traceTurnID(fs, args)
	if err != nil {
		return err
	}

	client, closeConn, err := traceClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.ReadTurn(ctx, &lobslawv1.ReadTurnRequest{TurnId: turnID})
	if err != nil {
		return err
	}

	source := traceSource(res.GetNodeId(), node.addr)
	if !res.GetEnabled() {
		return fmt.Errorf("%s: %s", source, tracingOffNote)
	}
	if len(res.GetSpans()) == 0 {
		// Names the node it asked, because "served on another node" is
		// the likeliest explanation and the operator needs to know
		// which one they just ruled out.
		return fmt.Errorf("%s recorded no spans for turn %q — it may have been served on another node, "+
			"or rotated out", source, turnID)
	}

	spans := make([]trace.Span, 0, len(res.GetSpans()))
	for _, p := range res.GetSpans() {
		spans = append(spans, trace.SpanFromProto(p))
	}
	sort.SliceStable(spans, func(i, j int) bool {
		return spans[i].StartedAt.Before(spans[j].StartedAt)
	})
	renderTurn(os.Stdout, source, turnID, spans)
	return nil
}
