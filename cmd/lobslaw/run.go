package main

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// Booting a node is a command you type, not what happens by default.
//
// Every unrecognised invocation used to fall through to "start a whole
// assistant". That is how `lobslaw nodeid` — documented, dispatched by
// nothing — booted a second node on somebody's machine when they
// followed the getting-started guide. The dispatch table fixed the
// hole for names we know about; this fixes the shape that made a hole
// dangerous.
//
// So: `lobslaw run` starts the node, and a bare `lobslaw` prints what
// it can do. A typo now costs a usage message rather than a process.

// runNames are what starts a node. "serve" because half the world
// calls it that and being wrong about which half costs an alias.
var runNames = []string{"run", "serve"}

// dispatchRun consumes the run/serve verb so the node path sees the
// remaining arguments exactly as it did when there was no verb at all.
//
// Returns the args to carry on with, and whether a node should start.
func dispatchRun(args []string) ([]string, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// A flag taking a separate value: skip the value too, or
			// `--config x.toml` reads x.toml as a positional and the
			// whole invocation looks like somebody else's command.
			// Same table findSubcmd uses, so the two cannot disagree
			// about what a flag is.
			if globalValueFlags[a] {
				i++
			}
			continue
		}
		if slices.Contains(runNames, a) {
			out := make([]string, 0, len(args)-1)
			out = append(out, args[:i]...)
			out = append(out, args[i+1:]...)
			return out, true
		}
		// A positional that is not a run verb: not ours.
		return args, false
	}
	// Flags only, no positional. Historically this booted a node, and
	// `lobslaw --config x` is in enough scripts and unit files that
	// breaking it would be a worse trade than the ambiguity it costs.
	return args, len(args) > 0
}

// printCommandList is what a bare `lobslaw` prints.
func printCommandList(w *os.File) {
	names := make([]string, 0, len(topLevelDispatchers())+1)
	for _, d := range topLevelDispatchers() {
		names = append(names, d.name)
	}
	sort.Strings(names)

	_, _ = fmt.Fprintln(w, "lobslaw — a personal agent cluster")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "usage:")
	_, _ = fmt.Fprintln(w, "  lobslaw run [--config config.toml]   start a node")
	_, _ = fmt.Fprintln(w, "  lobslaw <command> [args]             everything else")
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "commands:")
	for _, n := range names {
		if d, ok := commandSummaries[n]; ok {
			_, _ = fmt.Fprintf(w, "  %-14s %s\n", n, d)
			continue
		}
		_, _ = fmt.Fprintf(w, "  %s\n", n)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "run `lobslaw <command>` with no arguments for that command's own usage.")
}

// commandSummaries is one line per top-level command.
//
// A map rather than a field on topLevelCommand so the dispatch table
// stays the routing table: a test compares its KEYS against the
// dispatchers, which is what catches a command added without a
// description or a description outliving its command.
var commandSummaries = map[string]string{
	"cluster":      "certificates, node signing, operator export",
	"plugin":       "install and manage plugins",
	"audit":        "query and verify the audit chain",
	"skills":       "list and inspect installed skills",
	"trace":        "what a turn did, and what it cost",
	"learned":      "what the agent taught itself",
	"identity":     "principals and channel aliases",
	"policy":       "rules and pending approvals",
	"grants":       "standing approvals a conversation gave",
	"memory":       "browse, search and forget records",
	"session":      "stored conversations",
	"init":         "write a starting config",
	"enrol":        "ask a cluster for an operator credential",
	"nodeid":       "print this machine's node id",
	"context":      "named clusters this CLI can reach",
	"doctor":       "check a node's configuration and reachability",
	"embed-eval":   "measure an embedding model against this node's own memories",
	"sandbox-exec": "internal reexec helper; not typed by hand",
}
