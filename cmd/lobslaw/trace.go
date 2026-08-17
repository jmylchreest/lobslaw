package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/jmylchreest/lobslaw/internal/trace"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Reading a turn back.
//
// Traces are per-node files, so this reads the local node's directory
// directly rather than talking to a cluster. That is the honest shape:
// a turn served on another node was traced on that node, and pretending
// otherwise would produce a command that silently returns nothing for
// half the turns you ask about.

const traceUsage = `lobslaw trace — what a turn did, and what it cost

  trace list [--limit N]     turns recorded on a node, newest first
  trace <turn-id>            the spans of one turn

Talks to a RUNNING node over mTLS by default — use --context, or --addr
with the credential flags. Pass --offline to read a trace directory on
this machine instead.

Traces are PER-NODE files. A turn served on another node was traced
there, not here, so every answer names the node it came from — and
--offline names the directory, because a copy on a laptop is not the
cluster's.

Enable with:

  [trace]
  enabled = true

No span carries message text, tool arguments or tool output.`

// traceForms pairs each subcommand's live and offline implementation.
//
// A table rather than a switch so the ROUTING is a value a test can
// assert. The bug worth catching is `trace list --context prod`
// quietly reading a directory on the laptop and printing it as
// production's.
var traceForms = map[string]struct{ live, offline func([]string) error }{
	"list": {live: traceListLive, offline: traceList},
	// The turn-id form has no subcommand name; "show" is the internal
	// label for it and never typed.
	"show": {live: traceShowLive, offline: traceShow},
}

// traceRoute returns the implementation, or nil if there is none.
// Live is the default; --offline is the opt-out.
func traceRoute(sub string, offline bool) func([]string) error {
	form, ok := traceForms[sub]
	if !ok {
		return nil
	}
	if offline {
		return form.offline
	}
	return form.live
}

func dispatchTrace(args []string) bool {
	idx := findSubcmd(args, "trace")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, traceUsage)
		os.Exit(2)
	}

	rest, offline := takeOffline(sub)
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, traceUsage)
		os.Exit(2)
	}

	// "list" is the only named subcommand; anything else is a turn id,
	// which the show form takes as its first positional.
	name, showArgs := "show", rest
	if rest[0] == "list" {
		name, showArgs = "list", rest[1:]
	}

	if err := traceRoute(name, offline)(showArgs); err != nil {
		fmt.Fprintf(os.Stderr, "trace: %v\n", err)
		os.Exit(1)
	}
	return true
}

// traceDir resolves where traces live, mirroring the node's own
// resolution so the CLI and the node cannot disagree about the path.
func traceDir(fs *flag.FlagSet) (*string, *string) {
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""),
		"path to config.toml; supplies [cluster] data_dir and [trace] dir")
	dir := fs.String("dir", envOr("LOBSLAW_TRACE_DIR", ""),
		"explicit trace directory; overrides --config")
	return cfgPath, dir
}

func resolveTraceDir(cfgPath, dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if cfgPath == "" {
		return "", fmt.Errorf("no --dir and no --config to read one from")
	}
	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		return "", fmt.Errorf("load config %q: %w", cfgPath, err)
	}
	if cfg.Trace.Dir != "" {
		return cfg.Trace.Dir, nil
	}
	if cfg.Cluster.DataDir == "" {
		return "", fmt.Errorf("config has neither [trace] dir nor [cluster] data_dir")
	}
	// "traces" duplicated from internal/node rather than exported: a
	// constant shared between a CLI and a server is a coupling that
	// outlives the reason for it, and the path is in the docs either
	// way.
	return filepath.Join(cfg.Cluster.DataDir, "traces"), nil
}

func traceList(args []string) error {
	fs := flag.NewFlagSet("trace list", flag.ExitOnError)
	cfgPath, dir := traceDir(fs)
	limit := fs.Int("limit", 20, "how many turns to list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolved, err := resolveTraceDir(*cfgPath, *dir)
	if err != nil {
		return err
	}
	ids, err := trace.ListTurns(resolved, *limit)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		// Distinguished from an error. A node with tracing off and a
		// node that has served no turns both have nothing to show, and
		// neither is a failure.
		fmt.Printf("no turns recorded in %s\n", resolved)
		return nil
	}
	fmt.Printf("local: %s\n", resolved)
	for _, id := range ids {
		fmt.Println(id)
	}
	return nil
}

func traceShow(args []string) error {
	// Guarded rather than trusted. The dispatcher only calls this
	// with a turn id present, but a panic is a worse way to learn
	// that changed than an error is.
	if len(args) == 0 {
		return errors.New("a turn id is required: lobslaw trace <turn-id>")
	}
	fs := flag.NewFlagSet("trace", flag.ExitOnError)
	cfgPath, dir := traceDir(fs)
	// The turn id is positional and comes first, so parse the rest.
	turnID := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	resolved, err := resolveTraceDir(*cfgPath, *dir)
	if err != nil {
		return err
	}
	spans, err := trace.ReadTurn(resolved, turnID)
	if err != nil {
		return err
	}
	if len(spans) == 0 {
		return fmt.Errorf("no spans for turn %q in %s — it may have been served on another node, "+
			"or rotated out", turnID, resolved)
	}
	sort.SliceStable(spans, func(i, j int) bool {
		return spans[i].StartedAt.Before(spans[j].StartedAt)
	})
	renderTurn(os.Stdout, "local: "+resolved, turnID, spans)
	return nil
}

// renderTurn prints one turn's spans with a total.
//
// The total is the point of the command. A list of spans answers "what
// happened"; the total answers "why did that cost what it did", which
// is the question somebody opened this for.
//
// Source names WHERE the spans came from — a node id, or a directory.
// Traces are per-node, so a turn's cost without an attribution is a
// number about an unspecified machine.
func renderTurn(out io.Writer, source, turnID string, spans []trace.Span) {
	var totalCost, attributed float64
	var totalDur time.Duration
	var prompt, completion, cached int
	for _, s := range spans {
		totalDur += s.Duration
		// A context-carry span ATTRIBUTES cost the LLM spans have
		// already counted; it does not add any. Summing both would
		// double the turn's token count and roughly double its cost —
		// which would make the one command whose job is to answer "why
		// did this cost what it did" answer it wrongly.
		if s.Kind == trace.KindContextCarry {
			attributed += s.CostUSD
			continue
		}
		totalCost += s.CostUSD
		prompt += s.Usage.PromptTokens
		completion += s.Usage.CompletionTokens
		cached += s.Usage.CachedTokens
	}

	_, _ = fmt.Fprintf(out, "%s\n", source)
	_, _ = fmt.Fprintf(out, "turn %s — %d spans, %s, $%.4f\n", turnID, len(spans),
		totalDur.Round(time.Millisecond), totalCost)
	if attributed > 0 {
		// Stated as a share of the total, not added to it. This is the
		// number R24 exists for: in an agentic turn it is usually the
		// dominant cost and is currently attributable to nothing.
		share := 0.0
		if totalCost > 0 {
			share = attributed / totalCost * 100
		}
		_, _ = fmt.Fprintf(out, "  of which $%.4f (%.0f%%) is re-sent tool output\n", attributed, share)
	}
	_, _ = fmt.Fprintln(out)

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "KIND\tPROVIDER\tNAME\tTRY\tOUTCOME\tDURATION\tTOKENS\tCOST\tDETAIL")
	for _, s := range spans {
		tokens := ""
		if s.Usage.PromptTokens+s.Usage.CompletionTokens > 0 {
			tokens = fmt.Sprintf("%d/%d", s.Usage.PromptTokens, s.Usage.CompletionTokens)
			if s.Usage.CachedTokens > 0 {
				tokens += fmt.Sprintf(" (%d cached)", s.Usage.CachedTokens)
			}
		}
		if s.Unit != "" {
			// A non-token-billed call carries its own unit. A token
			// count of zero on a call that cost money reads as free.
			//
			// A context carry has both: the tokens it contributed and
			// the number of prompts it rode in. Showing only one hides
			// the calculation — "40020 tokens" looks like a big tool,
			// and "5 resends" looks like nothing at all.
			if s.Usage.PromptTokens > 0 {
				tokens = fmt.Sprintf("%d over %g %s", s.Usage.PromptTokens, s.Quantity, s.Unit)
			} else {
				tokens = fmt.Sprintf("%g %s", s.Quantity, s.Unit)
			}
		}
		cost := ""
		if s.CostUSD > 0 {
			cost = fmt.Sprintf("$%.4f", s.CostUSD)
		}
		dur := ""
		if s.Duration > 0 {
			dur = s.Duration.Round(time.Millisecond).String()
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n",
			s.Kind, s.Provider, s.Name, s.Attempt, s.Outcome, dur, tokens, cost, s.Error)
	}
	_ = w.Flush()

	if cached > 0 {
		_, _ = fmt.Fprintf(out, "\ntokens: %d prompt (%d cached), %d completion\n", prompt, cached, completion)
	} else if prompt+completion > 0 {
		_, _ = fmt.Fprintf(out, "\ntokens: %d prompt, %d completion\n", prompt, completion)
	}
}
