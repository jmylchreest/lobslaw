package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Reading and pruning memory from a laptop.
//
// `memory list` and `memory show` opened state.db directly, which
// needs the node STOPPED — and on an operator's laptop there is no
// state.db to open at all. An empty listing then reads as an empty
// cluster, which is the confidently-wrong answer R28 exists to remove.
//
// The scan itself lives in internal/memory now, so the live and
// offline forms answer the same question with one definition of what
// each filter means.

func memoryClient(node *liveNode) (lobslawv1.MemoryServiceClient, func(), error) {
	conn, err := node.dial()
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewMemoryServiceClient(conn), func() { _ = conn.Close() }, nil
}

// --- list --------------------------------------------------------------

func memoryListLive(args []string) error {
	fs := flag.NewFlagSet("memory list", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	var filter memory.RecordFilter
	fs.StringVar(&filter.Kind, "kind", "all", "which records to list: all|vector|episodic")
	fs.StringVar(&filter.Owner, "owner", "", "only records with this exact owner")
	fs.StringVar(&filter.Scope, "scope", "", "only vector records with this scope")
	fs.StringVar(&filter.Tag, "tag", "", "only episodic records carrying this tag")
	fs.BoolVar(&filter.Unowned, "unowned", false, "only records with no owner")
	fs.IntVar(&filter.Limit, "limit", 0, "cap records shown per kind (0 = no cap), newest first")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Validated here as well as at the server, so a mistyped --kind
	// costs no round trip.
	if err := filter.Validate(); err != nil {
		return err
	}

	client, closeConn, err := memoryClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.ListRecords(ctx, &lobslawv1.ListRecordsRequest{
		Kind:    filter.Kind,
		Owner:   filter.Owner,
		Scope:   filter.Scope,
		Tag:     filter.Tag,
		Unowned: filter.Unowned,
		Limit:   int32(filter.Limit), //nolint:gosec // a CLI --limit is not attacker-controlled
	})
	if err != nil {
		return err
	}

	page := recordPage{
		vectors:   res.GetVectors(),
		episodics: res.GetEpisodics(),
		totalV:    int(res.GetVectorTotal()),
		totalE:    int(res.GetEpisodicTotal()),
		unowned:   int(res.GetUnowned()),
	}
	if *asJSON {
		out := page.json()
		out["source"] = node.addr
		return emitJSON(out)
	}
	fmt.Println(node.addr)
	page.print()
	return nil
}

// --- show --------------------------------------------------------------

func memoryShowLive(args []string) error {
	fs := flag.NewFlagSet("memory show", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("exactly one record id required")
	}

	client, closeConn, err := memoryClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.GetRecord(ctx, &lobslawv1.GetRecordRequest{Id: positional[0]})
	if err != nil {
		return err
	}

	rec := recordFromResponse(res)
	if rec == nil {
		// The service answers NOT_FOUND, so reaching here means it
		// returned a success carrying neither record — a protocol
		// mismatch rather than a missing id, and saying "no such
		// record" would send somebody hunting for a typo.
		return fmt.Errorf("the node returned a record that is neither vector nor episodic")
	}
	if *asJSON {
		return emitJSON(map[string]any{
			"source":        node.addr,
			"kind":          rec.kind(),
			"bucket":        rec.bucket,
			"record":        rec.fields(),
			"referenced_by": res.GetReferencedBy(),
		})
	}
	fmt.Println(node.addr)
	printRecord(os.Stdout, rec, res.GetReferencedBy())
	return nil
}

// recordFromResponse picks whichever record the node sent, or nil.
func recordFromResponse(res *lobslawv1.GetRecordResponse) *memRecord {
	switch {
	case res.GetVector() != nil:
		return &memRecord{bucket: memory.BucketVectorRecords, vector: res.GetVector()}
	case res.GetEpisodic() != nil:
		return &memRecord{bucket: memory.BucketEpisodicRecords, episodic: res.GetEpisodic()}
	}
	return nil
}

// printRecord renders one record and what forgetting it would sweep.
//
// Shared by both forms, so a field that exists offline cannot go
// missing live — the two would otherwise drift into showing different
// things about the same record.
func printRecord(w io.Writer, rec *memRecord, refs []string) {
	_, _ = fmt.Fprintf(w, "%s %s\n", rec.kind(), rec.id())
	fields := rec.fields()
	for _, k := range rec.fieldOrder() {
		v := fields[k]
		if isEmptyField(v) {
			continue
		}
		_, _ = fmt.Fprintf(w, "  %-12s %v\n", k+":", v)
	}
	if rec.owner() == "" {
		_, _ = fmt.Fprintln(w, "  "+unownedNote)
	}
	if len(refs) > 0 {
		_, _ = fmt.Fprintf(w,
			"\n  referenced by %d consolidation(s) — forgetting this record sweeps them too:\n", len(refs))
		for _, r := range refs {
			_, _ = fmt.Fprintf(w, "    %s\n", r)
		}
	}
}

// --- forget ------------------------------------------------------------

func memoryForgetLive(args []string) error {
	fs := flag.NewFlagSet("memory forget", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	var ids stringList
	fs.Var(&ids, "id", "record id to forget (repeatable)")
	query := fs.String("query", "", "substring matched against vector text and episodic event/context")
	before := fs.String("before", "", "only records older than this (RFC3339 or YYYY-MM-DD)")
	var tags stringList
	fs.Var(&tags, "tag", "match records carrying this tag (repeatable)")
	requester := fs.String("requester", "", "run as this principal; records it may not read are left alone")
	apply := fs.Bool("apply", false, "actually delete; without it this is a dry run")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	req, err := forgetRequest(ids, *query, *before, tags, *requester, *apply)
	if err != nil {
		return err
	}

	client, closeConn, err := memoryClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.Forget(ctx, req)
	if err != nil {
		return err
	}

	if *asJSON {
		return emitJSON(map[string]any{
			"applied": *apply,
			"source":  node.addr,
			"matched": res.GetMatched(),
			"swept":   res.GetSwept(),
			"missing": res.GetMissing(),
			"total":   len(res.GetMatched()) + len(res.GetSwept()),
		})
	}
	printForgetPlan(os.Stdout, node.addr,
		res.GetMatched(), res.GetSwept(), res.GetMissing(), *apply)
	return nil
}

// forgetRequest turns the flags into a request.
//
// DryRun is the INVERSE of --apply, which is the one bit of this
// command that must never invert: forget cascades through SourceIds
// and is irreversible, so a "dry run" that deletes has no undo.
//
// The empty-filter refusal is here as well as at the server, so an
// unfiltered forget never leaves the laptop — an empty query matches
// every record in the store.
func forgetRequest(ids []string, query, before string, tags []string,
	requester string, apply bool) (*lobslawv1.ForgetRequest, error) {
	req := &lobslawv1.ForgetRequest{
		Ids:       ids,
		Query:     query,
		Tags:      tags,
		Requester: requester,
		DryRun:    !apply,
	}
	if before != "" {
		t, err := parseBefore(before)
		if err != nil {
			return nil, err
		}
		req.Before = timestamppb.New(t)
	}
	if len(req.Ids) == 0 && req.Query == "" && req.Before == nil && len(req.Tags) == 0 {
		return nil, errors.New(
			"at least one of --id, --query, --before or --tag is required — refusing to forget everything")
	}
	return req, nil
}

// printForgetPlan reports the blast radius, and whether it happened.
//
// Shared by both forms so the dry run an operator reads offline is
// word-for-word the one they read live — this is the output somebody
// decides an irreversible delete on.
func printForgetPlan(w io.Writer, source string, matched, swept, missing []string, applied bool) {
	total := len(matched) + len(swept)
	_, _ = fmt.Fprintf(w, "%s\n", source)
	_, _ = fmt.Fprintf(w, "matched:   %d record(s)\n", len(matched))
	fprintSample(w, matched)
	_, _ = fmt.Fprintf(w, "cascade:   %d consolidation(s) whose sources are in the matched set\n", len(swept))
	fprintSample(w, swept)
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(w, "not found: %d requested id(s) — %s\n", len(missing), strings.Join(missing, ", "))
	}
	_, _ = fmt.Fprintf(w, "total:     %d record(s)\n", total)

	switch {
	case total == 0:
		_, _ = fmt.Fprintln(w, "\nnothing to do.")
	case applied:
		_, _ = fmt.Fprintf(w, "\nDELETED %d record(s).\n", total)
	default:
		_, _ = fmt.Fprintln(w, "\nDRY RUN — nothing was written. Re-run with --apply to delete.")
	}
}
