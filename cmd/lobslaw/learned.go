package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// "Show me everything it decided on its own" and "forget everything
// you taught yourself" are one scan and one operation each, because
// provenance is a location rather than a tag. That is the payoff of
// the separate store, and these are where an operator collects it.

const learnedUsage = `lobslaw learned — inspect and manage what the agent taught itself

Most subcommands talk to a RUNNING node over mTLS. Pass --offline to
open state.db directly instead — that path needs the node STOPPED,
because bbolt takes an exclusive lock on the file, and it exists for
reading a cluster that will not start.

Approving a proposal is routine, so it should not require an outage.

subcommands:
  list                 what the agent has written for itself
  show <id>            read one artefact in full, before deciding on it
  archive <id>...      move artefacts out of the live set, recoverably
  discard              archive everything (except pinned artefacts) [offline]
  restore <id>...      bring archived artefacts back, as proposals
  pending              refinements staged against a live artefact
  accept <id>...       apply a staged refinement
  reject <id>...       discard a staged refinement, leaving the live one
  history <id>         prior versions kept for rollback
  rollback <id> <ver>  restore a prior version as the current one
  approve <id>...      let a proposal out of PROPOSED (live only)

Subcommands marked [offline] have no live form yet. They still run, and
they say so — a command that quietly read a local state.db would be
reporting on a store the cluster never wrote.

Nothing here deletes. Archived artefacts stay readable with
--archived — an agent that can silently erase evidence of what it
taught itself is the wrong default.

archive, discard and restore are DRY RUN unless --apply is given.`

// learnedForms pairs each subcommand's live and offline
// implementation. A table rather than a switch so the ROUTING is a
// value a test can assert.
var learnedForms = map[string]struct{ live, offline func([]string) error }{
	"list":     {live: liveList, offline: learnedList},
	"pending":  {live: livePending, offline: learnedPending},
	"accept":   {live: func(a []string) error { return liveDecide(a, true) }, offline: learnedAccept},
	"reject":   {live: func(a []string) error { return liveDecide(a, false) }, offline: learnedReject},
	"archive":  {live: liveShelve, offline: learnedArchive},
	"restore":  {live: liveRestore, offline: learnedRestore},
	"history":  {live: liveHistory, offline: learnedHistory},
	"rollback": {live: liveRollback, offline: learnedRollback},
}

// learnedOfflineOnly are the subcommands with no live form.
//
// discard is a bulk archive with a dry run over the whole live set.
// Composing it out of per-artefact calls would make the preview and
// the writes two different reads of the store, which for a command
// that archives everything is the wrong place to be approximate.
var learnedOfflineOnly = map[string]func([]string) error{
	"discard": learnedDiscard,
}

// learnedLiveOnly are the subcommands with no offline form.
var learnedLiveOnly = map[string]func([]string) error{
	"approve": liveApprove,
	// show has no offline form because it does not need one: the
	// offline path exists for reading a cluster that will not start,
	// and reading a proposal is something you do in order to approve
	// it — which is live-only anyway.
	"show": learnedShowLive,
}

// learnedRoute resolves a subcommand.
//
// liveMissing is set when the caller did not ask for --offline and the
// subcommand has no live form: it runs, and the caller announces the
// gap. Refusing a command that works to make a point about a flag
// would be worse; running it silently is the failure R28 names.
func learnedRoute(sub string, offline bool) (fn func([]string) error, liveMissing bool, err error) {
	if form, ok := learnedForms[sub]; ok {
		if offline {
			return form.offline, false, nil
		}
		return form.live, false, nil
	}
	if fn, ok := learnedOfflineOnly[sub]; ok {
		return fn, !offline, nil
	}
	if fn, ok := learnedLiveOnly[sub]; ok {
		if offline {
			// Named rather than silently ignoring --offline: somebody who
			// passed it believes the node is stopped, and approving
			// against a store they think is quiescent is exactly the
			// misunderstanding worth stopping.
			return nil, false, errLiveOnly(sub)
		}
		return fn, false, nil
	}
	return nil, false, nil
}

func dispatchLearned(args []string) bool {
	idx := findSubcmd(args, "learned")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, learnedUsage)
		os.Exit(2)
	}

	// Live is the default and --offline is the opt-out, not the other
	// way round. Approving a proposal is routine; reading a cluster
	// that will not start is not, and the common case should not be
	// the one that needs a flag.
	rest, offline := takeOffline(sub[1:])

	run, liveMissing, err := learnedRoute(sub[0], offline)
	switch {
	case err != nil:
		fmt.Fprintf(os.Stderr, "learned %s: %v\n", sub[0], err)
		os.Exit(1)
	case run == nil:
		fmt.Fprintf(os.Stderr, "unknown learned subcommand %q\n\n%s\n", sub[0], learnedUsage)
		os.Exit(2)
	}
	if liveMissing {
		fmt.Fprintf(os.Stderr,
			"lobslaw learned %s: no live form yet — running against a local state.db, "+
				"which is NOT the cluster's unless this machine is the node\n", sub[0])
	}
	if err := run(rest); err != nil {
		fmt.Fprintf(os.Stderr, "learned %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

func learnedList(args []string) error {
	fs := flag.NewFlagSet("learned list", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	archived := fs.Bool("archived", false, "read the archive instead of the live set")
	owner := fs.String("owner", "", "restrict to one principal")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	st := memory.NewOfflineSelfTaught(s)
	records, err := st.List(*archived, *owner)
	if err != nil {
		return err
	}

	if *asJSON {
		out := make([]map[string]any, 0, len(records))
		for _, r := range records {
			out = append(out, learnedJSON(r, st))
		}
		return emitJSON(map[string]any{"source": path, "artefacts": out})
	}
	return renderLearnedList(os.Stdout, records, path, *archived, false)
}

// renderLearnedList prints the artefact set and SAYS WHERE IT CAME
// FROM.
//
// "The agent has taught itself nothing" is indistinguishable from the
// wrong store unless the source is on the page — and on a laptop that
// sentence used to be about a state.db the cluster never wrote.
//
// Usage counts are omitted on the live path: they live in a separate
// bucket the RPC does not return, and a zero next to every artefact
// would read as "never used" rather than "not asked for".
func renderLearnedList(w io.Writer, records []*lobslawv1.SelfTaughtRecord,
	source string, archived, asJSON bool) error {
	if asJSON {
		out := make([]map[string]any, 0, len(records))
		for _, r := range records {
			out = append(out, learnedJSON(r, nil))
		}
		return emitJSON(map[string]any{"source": source, "artefacts": out})
	}

	_, _ = fmt.Fprintf(w, "%s\n", source)
	if len(records) == 0 {
		if archived {
			_, _ = fmt.Fprintln(w, "the archive is empty.")
		} else {
			_, _ = fmt.Fprintln(w, "the agent has taught itself nothing.")
		}
		return nil
	}
	for _, r := range records {
		pin := ""
		if r.GetPinned() {
			pin = " [pinned]"
		}
		_, _ = fmt.Fprintf(w, "  %-36s %-10s %-9s%s\n",
			r.GetId(), kindLabel(r.GetKind()), stateLabel(r.GetState()), pin)
		if r.GetArchivedReason() != "" {
			_, _ = fmt.Fprintf(w, "      archived: %s\n", r.GetArchivedReason())
		}
		if r.GetPending() != nil {
			_, _ = fmt.Fprintf(w, "      PENDING refinement to v%d: %s\n",
				r.GetVersion()+1, r.GetPending().GetRationale())
		}
		if r.GetTurnId() != "" {
			_, _ = fmt.Fprintf(w, "      taught by turn %s\n", r.GetTurnId())
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d artefact(s).\n", len(records))
	return nil
}

func learnedArchive(args []string) error {
	return mutateLearned("learned archive", args, func(st *memory.OfflineSelfTaught, ids []string, apply bool) error {
		if len(ids) == 0 {
			return fmt.Errorf("name at least one artefact id (see: lobslaw learned list)")
		}
		return archiveIDs(st, ids, apply, "archived by operator")
	})
}

func learnedDiscard(args []string) error {
	return mutateLearned("learned discard", args, func(st *memory.OfflineSelfTaught, _ []string, apply bool) error {
		live, err := st.List(false, "")
		if err != nil {
			return err
		}
		var ids []string
		for _, r := range live {
			if r.Pinned {
				// Said out loud rather than silently skipped: an
				// operator running "discard" and finding something
				// still there deserves to know why.
				fmt.Printf("  SKIPPED %-30s (pinned)\n", r.Id)
				continue
			}
			ids = append(ids, r.Id)
		}
		return archiveIDs(st, ids, apply, "discarded by operator")
	})
}

func learnedRestore(args []string) error {
	return mutateLearned("learned restore", args, func(st *memory.OfflineSelfTaught, ids []string, apply bool) error {
		if len(ids) == 0 {
			return fmt.Errorf("name at least one artefact id (see: lobslaw learned list --archived)")
		}
		for _, id := range ids {
			rec, archived, err := st.Find(id)
			if err != nil {
				fmt.Printf("  NOT FOUND %s\n", id)
				continue
			}
			if !archived {
				fmt.Printf("  ALREADY LIVE %s\n", id)
				continue
			}
			fmt.Printf("  %-36s restore as proposed\n", id)
			if apply {
				if err := st.Restore(rec); err != nil {
					return fmt.Errorf("%s: %w", id, err)
				}
			}
		}
		return nil
	})
}

func archiveIDs(st *memory.OfflineSelfTaught, ids []string, apply bool, reason string) error {
	for _, id := range ids {
		rec, archived, err := st.Find(id)
		if err != nil {
			fmt.Printf("  NOT FOUND %s\n", id)
			continue
		}
		if archived {
			fmt.Printf("  ALREADY ARCHIVED %s\n", id)
			continue
		}
		fmt.Printf("  %-36s archive\n", id)
		if apply {
			if err := st.Archive(rec, reason); err != nil {
				return fmt.Errorf("%s: %w", id, err)
			}
		}
	}
	return nil
}

func mutateLearned(name string, args []string, fn func(*memory.OfflineSelfTaught, []string, bool) error) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	apply := fs.Bool("apply", false, "actually write (default is a dry run)")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}

	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	fmt.Printf("%s\n", path)
	if err := fn(memory.NewOfflineSelfTaught(s), positional, *apply); err != nil {
		return err
	}
	if !*apply {
		fmt.Println("\nDRY RUN — nothing was written. Re-run with --apply.")
	}
	return nil
}

// learnedPending lists refinements waiting on a decision.
//
// Separate from `list` because they are a different question: `list`
// asks what the agent has, `pending` asks what it wants to change —
// and the second is the one somebody has to act on.
func learnedPending(args []string) error {
	fs := flag.NewFlagSet("learned pending", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	records, err := memory.NewOfflineSelfTaught(s).List(false, "")
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", path)
	var n int
	for _, r := range records {
		if r.Pending == nil {
			continue
		}
		n++
		fmt.Printf("  %-36s v%d -> v%d\n", r.Id, r.Version, r.Version+1)
		fmt.Printf("      rationale: %s\n", r.Pending.Rationale)
		if r.Pending.Description != "" && r.Pending.Description != r.Description {
			fmt.Printf("      description: %q -> %q\n", r.Description, r.Pending.Description)
		}
		if r.Pending.TurnId != "" {
			fmt.Printf("      proposed by turn %s\n", r.Pending.TurnId)
		}
	}
	if n == 0 {
		fmt.Println("nothing is waiting on a decision.")
		return nil
	}
	fmt.Printf("\n%d refinement(s). Apply with: lobslaw learned accept <id> --apply\n", n)
	return nil
}

func learnedAccept(args []string) error {
	return mutateLearned("learned accept", args, func(st *memory.OfflineSelfTaught, ids []string, apply bool) error {
		return decidePending(st, ids, apply, true)
	})
}

func learnedReject(args []string) error {
	return mutateLearned("learned reject", args, func(st *memory.OfflineSelfTaught, ids []string, apply bool) error {
		return decidePending(st, ids, apply, false)
	})
}

func decidePending(st *memory.OfflineSelfTaught, ids []string, apply, accept bool) error {
	if len(ids) == 0 {
		return fmt.Errorf("name at least one artefact id (see: lobslaw learned pending)")
	}
	verb := "reject"
	if accept {
		verb = "accept"
	}
	for _, id := range ids {
		rec, _, err := st.Find(id)
		if err != nil {
			fmt.Printf("  NOT FOUND %s\n", id)
			continue
		}
		if rec.Pending == nil {
			fmt.Printf("  NOTHING PENDING %s\n", id)
			continue
		}
		fmt.Printf("  %-36s %s v%d -> v%d\n", id, verb, rec.Version, rec.Version+1)
		if !apply {
			continue
		}
		if accept {
			err = st.ApprovePending(rec, "operator")
		} else {
			err = st.RejectPending(rec)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	return nil
}

func learnedHistory(args []string) error {
	fs := flag.NewFlagSet("learned history", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return fmt.Errorf("usage: lobslaw learned history <id>")
	}
	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	st := memory.NewOfflineSelfTaught(s)
	current, _, err := st.Find(positional[0])
	if err != nil {
		return err
	}
	versions, err := st.History(positional[0])
	if err != nil {
		return err
	}

	return renderLearnedHistory(os.Stdout, current, versions, path)
}

// renderLearnedHistory prints the versions and SAYS WHERE THEY CAME
// FROM. Shared by both forms so they cannot drift.
func renderLearnedHistory(w io.Writer, current *lobslawv1.SelfTaughtRecord,
	versions []*lobslawv1.SelfTaughtRecord, source string) error {
	_, _ = fmt.Fprintf(w, "%s\n", source)
	_, _ = fmt.Fprintf(w, "  v%-4d %-9s (current)  %s\n", current.GetVersion(),
		stateLabel(current.GetState()), firstLine(current.GetDescription()))
	for _, v := range versions {
		_, _ = fmt.Fprintf(w, "  v%-4d %-9s            %s\n", v.GetVersion(),
			stateLabel(v.GetState()), firstLine(v.GetDescription()))
	}
	if len(versions) == 0 {
		_, _ = fmt.Fprintln(w, "\nno prior versions retained.")
		return nil
	}
	_, _ = fmt.Fprintf(w, "\nRestore with: lobslaw learned rollback %s <version> --apply\n", current.GetId())
	return nil
}

func learnedRollback(args []string) error {
	fs := flag.NewFlagSet("learned rollback", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	apply := fs.Bool("apply", false, "actually write (default is a dry run)")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return fmt.Errorf("usage: lobslaw learned rollback <id> <version> [--apply]")
	}
	version, err := strconv.ParseUint(positional[1], 10, 32)
	if err != nil {
		return fmt.Errorf("version must be a number: %w", err)
	}

	s, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	st := memory.NewOfflineSelfTaught(s)
	current, _, err := st.Find(positional[0])
	if err != nil {
		return err
	}
	fmt.Printf("%s\n", path)
	fmt.Printf("  %-36s v%d -> restore v%d as v%d\n",
		positional[0], current.Version, version, current.Version+1)
	if !*apply {
		fmt.Println("\nDRY RUN — nothing was written. Re-run with --apply.")
		return nil
	}
	if err := st.Rollback(current, uint32(version)); err != nil {
		return err
	}
	fmt.Println("\nRolled back. The version you were on is still in history.")
	return nil
}

// firstLine keeps a listing to one row per version even when a
// description slipped past its single-line rule.
func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

// learnedJSON renders one artefact.
//
// st is nil on the live path: usage counts live in a separate bucket
// the RPC does not return, and "uses": 0 next to every artefact would
// read as "never used" rather than "not asked for". The key is omitted
// instead, which is the difference between an unknown and a zero.
func learnedJSON(r *lobslawv1.SelfTaughtRecord, st *memory.OfflineSelfTaught) map[string]any {
	m := map[string]any{
		"id":      r.Id,
		"kind":    kindLabel(r.Kind),
		"name":    r.Name,
		"state":   stateLabel(r.State),
		"origin":  originLabel(r.Origin),
		"owner":   r.Owner,
		"pinned":  r.Pinned,
		"version": r.Version,
		"turn_id": r.TurnId,
	}
	if st != nil {
		m["uses"] = st.Usage(r.Id).Invocations
	}
	if r.ArchivedReason != "" {
		m["archived_reason"] = r.ArchivedReason
	}
	if r.CreatedAt != nil {
		m["created_at"] = r.CreatedAt.AsTime()
	}
	return m
}

func kindLabel(k lobslawv1.SelfTaughtKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "SELF_TAUGHT_KIND_"))
}

func stateLabel(s lobslawv1.SelfTaughtState) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "SELF_TAUGHT_STATE_"))
}

func originLabel(o lobslawv1.SelfTaughtOrigin) string {
	return strings.ToLower(strings.TrimPrefix(o.String(), "SELF_TAUGHT_ORIGIN_"))
}

// takeOffline strips --offline from args and reports whether it was
// there.
//
// Pulled out before flag.Parse because each subcommand builds its own
// FlagSet, and which FlagSet to build is the thing this decides. A
// flag that selects the parser cannot be parsed by it.
func takeOffline(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	var offline bool
	for _, a := range args {
		if a == "--offline" || a == "-offline" {
			offline = true
			continue
		}
		out = append(out, a)
	}
	return out, offline
}

// errLiveOnly explains a subcommand that has no offline form.
//
// Named rather than silently ignoring --offline: somebody who passed
// it believes the node is stopped, and approving against a store they
// think is quiescent is exactly the misunderstanding worth stopping.
func errLiveOnly(name string) error {
	return fmt.Errorf(
		"%s has no --offline form: it is a decision, and recording one against a stopped "+
			"node would mean the running cluster never sees it", name)
}
