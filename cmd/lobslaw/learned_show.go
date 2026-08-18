package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Reading a proposal before deciding on it.
//
// `learned approve` existed and `learned show` did not, so the only
// way to see what you were approving was to stop the node and open
// state.db. For a mode whose entire purpose is that a person looks
// first, that is backwards: propose mode without a way to read a
// proposal is auto mode with extra steps.
//
// No new RPC. ListArtefacts already returns the whole record — body,
// files, description, and any pending refinement. The data was on the
// wire the whole time and only the rendering was missing.

func learnedShowLive(args []string) error {
	fs := flag.NewFlagSet("learned show", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	archived := fs.Bool("archived", false, "look in the archive instead of the live set")
	asJSON := fs.Bool("json", false, "emit JSON")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("exactly one artefact id is required: lobslaw learned show <id>")
	}
	id := positional[0]

	conn, err := node.dial()
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	client := lobslawv1.NewSelfLearningServiceClient(conn)

	ctx, cancel := node.ctx()
	defer cancel()
	resp, err := client.ListArtefacts(ctx, &lobslawv1.ListArtefactsRequest{Archived: *archived})
	if err != nil {
		return explainUnimplemented(err, node.addr)
	}

	rec := findArtefact(resp.GetArtefacts(), id)
	if rec == nil {
		// Names the other side, because an artefact proposed on
		// another node is the likeliest reason for a miss, and
		// "no such artefact" would send somebody hunting for a typo.
		where := "live set"
		if *archived {
			where = "archive"
		}
		return fmt.Errorf("%s: no artefact %q in the %s%s",
			node.addr, id, where, archivedHint(*archived))
	}
	if *asJSON {
		return emitJSON(artefactJSON(rec, node.addr))
	}
	fmt.Println(node.addr)
	renderArtefact(os.Stdout, rec)
	return nil
}

// archivedHint points at the other half of the store, once.
func archivedHint(archived bool) string {
	if archived {
		return " (the live set is the default; this looked in the archive)"
	}
	return " — try --archived"
}

func findArtefact(records []*lobslawv1.SelfTaughtRecord, id string) *lobslawv1.SelfTaughtRecord {
	for _, r := range records {
		if r.GetId() == id {
			return r
		}
	}
	// Names are unique per kind and are what a listing shows most
	// prominently, so accepting one saves retyping "skill:" for the
	// common case. Exact id wins above, so a name that collides with
	// an id can never shadow it.
	for _, r := range records {
		if r.GetName() == id {
			return r
		}
	}
	return nil
}

// renderArtefact prints everything a person needs in order to decide.
//
// The body in full, never truncated. A summary is what the listing is
// for; this is the command somebody runs when the summary was not
// enough, and a body cut off at 500 characters would be approved
// unread exactly as often as one that was never shown.
func renderArtefact(w io.Writer, r *lobslawv1.SelfTaughtRecord) {
	_, _ = fmt.Fprintf(w, "%s  %s  %s\n", r.GetId(), kindLabel(r.GetKind()), stateLabel(r.GetState()))
	if d := r.GetDescription(); d != "" {
		_, _ = fmt.Fprintf(w, "  %s\n", d)
	}
	_, _ = fmt.Fprintln(w)

	writeField(w, "taught by", r.GetTurnId())
	writeField(w, "owner", r.GetOwner())
	writeField(w, "origin", originLabel(r.GetOrigin()))
	writeField(w, "version", fmt.Sprintf("%d", r.GetVersion()))
	if r.GetApprovedBy() != "" {
		writeField(w, "approved by", r.GetApprovedBy())
	}
	if r.GetArchivedReason() != "" {
		writeField(w, "archived", r.GetArchivedReason())
	}
	if r.GetPinned() {
		writeField(w, "pinned", "yes — exempt from automatic archiving")
	}

	_, _ = fmt.Fprintf(w, "\n--- body ---\n%s\n", strings.TrimRight(r.GetBody(), "\n"))

	if files := r.GetFiles(); len(files) > 0 {
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			_, _ = fmt.Fprintf(w, "\n--- %s ---\n%s\n", name, strings.TrimRight(files[name], "\n"))
		}
	}

	// A pending refinement is the thing `learned accept` would apply,
	// and it is NOT what the body above says — the live artefact keeps
	// working while the refinement waits. Showing one without the
	// other would have somebody accept a change they had not read.
	if p := r.GetPending(); p != nil {
		_, _ = fmt.Fprintf(w, "\n=== pending refinement (accept applies THIS, not the body above) ===\n")
		writeField(w, "proposed by", p.GetTurnId())
		writeField(w, "rationale", p.GetRationale())
		if d := p.GetDescription(); d != "" {
			writeField(w, "description", d)
		}
		_, _ = fmt.Fprintf(w, "\n--- proposed body ---\n%s\n", strings.TrimRight(p.GetBody(), "\n"))
	}
}

func writeField(w io.Writer, name, value string) {
	if value == "" {
		return
	}
	_, _ = fmt.Fprintf(w, "  %-12s %s\n", name+":", value)
}

func artefactJSON(r *lobslawv1.SelfTaughtRecord, source string) map[string]any {
	out := map[string]any{
		"source":      source,
		"id":          r.GetId(),
		"kind":        kindLabel(r.GetKind()),
		"state":       stateLabel(r.GetState()),
		"name":        r.GetName(),
		"description": r.GetDescription(),
		"body":        r.GetBody(),
		"origin":      originLabel(r.GetOrigin()),
		"turn_id":     r.GetTurnId(),
		"owner":       r.GetOwner(),
		"version":     r.GetVersion(),
		"pinned":      r.GetPinned(),
	}
	if len(r.GetFiles()) > 0 {
		out["files"] = r.GetFiles()
	}
	if p := r.GetPending(); p != nil {
		out["pending"] = map[string]any{
			"body":        p.GetBody(),
			"description": p.GetDescription(),
			"rationale":   p.GetRationale(),
			"turn_id":     p.GetTurnId(),
			"files":       p.GetFiles(),
		}
	}
	return out
}
