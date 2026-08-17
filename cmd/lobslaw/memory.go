package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// memoryUsage is printed for a bare `lobslaw memory` and for an
// unknown subcommand.
const memoryUsage = `lobslaw memory — read and edit the memory store

show, list and forget talk to a RUNNING node over mTLS by default —
use --context, or --addr with the credential flags. Pass --offline to
open state.db directly instead; that path needs the node STOPPED,
because bbolt takes an exclusive lock, and it exists for reading a
cluster that will not start.

subcommands:
  show <id>        print one vector or episodic record in full
  list             list records, with filters
  forget <filter>  delete records AND the consolidations built from them
  share <id>...    make owned records readable cluster-wide (offline)
  unshare <id>...  return shared records to their owner only (offline)
  consolidations   what Dream merged, superseded or left alone (offline)

forget, share and unshare are DRY RUN unless --apply is given.

Adding records is deliberately absent: a memory needs an embedding to
be findable and the CLI has no embedder wired.`

// memoryForms pairs each subcommand's live and offline implementation.
//
// A table rather than a switch so the ROUTING is a value a test can
// assert. The bug worth catching is not a missing function — it is
// `list` quietly reading a laptop-local state.db and reporting an
// empty store as an empty cluster.
//
// share, unshare and consolidations are offline-only for now: they
// need RPCs that do not exist, and pretending otherwise would be worse
// than saying so.
var memoryForms = map[string]struct{ live, offline func([]string) error }{
	"show":   {live: memoryShowLive, offline: memoryShow},
	"list":   {live: memoryListLive, offline: memoryList},
	"forget": {live: memoryForgetLive, offline: memoryForget},
}

// memoryOfflineOnly are the subcommands with no live form yet.
var memoryOfflineOnly = map[string]func([]string) error{
	"share":          memoryShare,
	"unshare":        memoryUnshare,
	"consolidations": memoryConsolidations,
}

// memoryRoute returns the implementation for a subcommand, or nil if
// there is none. Live is the default; --offline is the opt-out.
//
// An offline-only subcommand runs offline whether or not the flag was
// given, and says so — the alternative is refusing a command that
// works, to make a point about a flag.
func memoryRoute(sub string, offline bool) (fn func([]string) error, liveMissing bool) {
	if form, ok := memoryForms[sub]; ok {
		if offline {
			return form.offline, false
		}
		return form.live, false
	}
	if fn, ok := memoryOfflineOnly[sub]; ok {
		return fn, !offline
	}
	return nil, false
}

// dispatchMemory handles `lobslaw memory <subcmd>`. Returns true if it
// handled the args.
func dispatchMemory(args []string) bool {
	idx := findSubcmd(args, "memory")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, memoryUsage)
		os.Exit(2)
	}

	rest, offline := takeOffline(sub[1:])
	run, liveMissing := memoryRoute(sub[0], offline)
	if run == nil {
		fmt.Fprintf(os.Stderr, "lobslaw memory: unknown subcommand %q\n\n", sub[0])
		fmt.Fprintln(os.Stderr, memoryUsage)
		os.Exit(2)
	}
	if liveMissing {
		// Announced rather than silent. Somebody who did not pass
		// --offline believes they are talking to the cluster, and a
		// command that quietly opened a local state.db instead is the
		// exact failure this work exists to remove.
		fmt.Fprintf(os.Stderr,
			"lobslaw memory %s: no live form yet — running against a local state.db, "+
				"which is NOT the cluster's unless this machine is the node\n", sub[0])
	}
	runOffline("memory "+sub[0], run, rest)
	return true
}

// runOffline is the seam between the dispatchers (which exit) and the
// subcommand bodies (which return errors). Every offline subcommand
// is a func(args) error so tests can drive it against a temp store and
// assert on the error instead of on a process exit.
func runOffline(name string, fn func(args []string) error, args []string) {
	if err := fn(args); err != nil {
		exitWith(fmt.Sprintf("%s: %v", name, err))
	}
}

func memoryShow(args []string) error {
	fs := flag.NewFlagSet("memory show", flag.ExitOnError)
	var opts offlineStore
	opts.bind(fs)
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("exactly one record id required")
	}
	id := fs.Arg(0)

	store, _, err := opts.open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	rec, err := loadMemRecord(store, id)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("no vector or episodic record with id %q", id)
	}

	// A record's consolidations are what a forget would take with it,
	// so they belong next to the record rather than a scan away.
	refs, err := referencedBy(store, id)
	if err != nil {
		return err
	}

	if *asJSON {
		return emitJSON(map[string]any{
			"kind":          rec.kind(),
			"bucket":        rec.bucket,
			"record":        rec.fields(),
			"referenced_by": refs,
		})
	}

	printRecord(os.Stdout, rec, refs)
	return nil
}

func memoryList(args []string) error {
	fs := flag.NewFlagSet("memory list", flag.ExitOnError)
	var opts offlineStore
	opts.bind(fs)
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
	if err := filter.Validate(); err != nil {
		return err
	}

	store, _, err := opts.open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	page, err := collectRecords(store, filter)
	if err != nil {
		return err
	}
	if *asJSON {
		return emitJSON(page.json())
	}
	page.print()
	return nil
}

// recordPage is a rendered-ready view of a scan. The scan itself lives
// in internal/memory, because MemoryService answers the same question
// and two implementations of "--unowned" would drift the day somebody
// fixed a filter in one of them.
type recordPage struct {
	vectors   []*lobslawv1.VectorRecord
	episodics []*lobslawv1.EpisodicRecord
	totalV    int
	totalE    int
	unowned   int
}

func pageFrom(p memory.RecordPage) recordPage {
	return recordPage{
		vectors:   p.Vectors,
		episodics: p.Episodics,
		totalV:    p.VectorTotal,
		totalE:    p.EpisodicTotal,
		unowned:   p.Unowned,
	}
}

func collectRecords(store *memory.Store, filter memory.RecordFilter) (recordPage, error) {
	page, err := memory.QueryRecords(store, filter)
	if err != nil {
		return recordPage{}, err
	}
	return pageFrom(page), nil
}

func (p recordPage) json() map[string]any {
	vj := make([]map[string]any, 0, len(p.vectors))
	for _, v := range p.vectors {
		vj = append(vj, vectorFields(v))
	}
	ej := make([]map[string]any, 0, len(p.episodics))
	for _, e := range p.episodics {
		ej = append(ej, episodicFields(e))
	}
	return map[string]any{
		"vector":   vj,
		"episodic": ej,
		"summary": map[string]any{
			"vector_total":   p.totalV,
			"episodic_total": p.totalE,
			"vector_shown":   len(p.vectors),
			"episodic_shown": len(p.episodics),
			"unowned":        p.unowned,
		},
	}
}

func (p recordPage) print() {
	if len(p.vectors) > 0 {
		fmt.Println("=== VECTOR RECORDS ===")
		for _, v := range p.vectors {
			fmt.Println(vectorLine(v))
		}
		fmt.Println()
	}
	if len(p.episodics) > 0 {
		fmt.Println("=== EPISODIC RECORDS ===")
		for _, e := range p.episodics {
			fmt.Println(episodicLine(e))
		}
		fmt.Println()
	}

	fmt.Printf("vector: %s   episodic: %s\n",
		shownOf(len(p.vectors), p.totalV), shownOf(len(p.episodics), p.totalE))
	if p.totalV+p.totalE == 0 {
		fmt.Println("(no records matched)")
	}
	fmt.Println("run `lobslaw memory show <id>` for a record's full text, context and metadata")
	if p.unowned > 0 {
		fmt.Printf("\n%d record(s) marked ! have no owner. %s\n", p.unowned, unownedNote)
	}
}

// unownedNote explains the ! marker. Ownership is stamped on every
// record written since it existed, so an unowned record today is a
// leftover or a write path that skipped the field — either way it is
// attributable to nobody, and share/unshare refuse to touch it.
const unownedNote = "unowned records belong to no principal — investigate before sharing or forgetting them"

func memoryForget(args []string) error {
	fs := flag.NewFlagSet("memory forget", flag.ExitOnError)
	var opts offlineStore
	opts.bind(fs)
	var ids stringList
	fs.Var(&ids, "id", "record id to forget (repeatable)")
	query := fs.String("query", "", "substring matched against vector text and episodic event/context")
	before := fs.String("before", "", "only records older than this (RFC3339 or YYYY-MM-DD)")
	var tags stringList
	fs.Var(&tags, "tag", "match records carrying this tag (repeatable)")
	apply := fs.Bool("apply", false, "actually delete; without it this is a dry run")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	q := memory.ForgetQuery{IDs: ids, Text: *query, Tags: tags}
	if *before != "" {
		t, err := parseBefore(*before)
		if err != nil {
			return err
		}
		q.Before = t
	}
	if q.IsEmpty() {
		// Same guard as Service.Forget: an unfiltered forget matches
		// every record in the store.
		return errors.New("at least one of --id, --query, --before or --tag is required — refusing to forget everything")
	}

	store, path, err := opts.open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	plan, err := memory.PlanForget(store, q)
	if err != nil {
		return err
	}

	if *apply && plan.Total() > 0 {
		if err := memory.ApplyForgetPlan(store, plan); err != nil {
			return err
		}
	}

	if *asJSON {
		return emitJSON(map[string]any{
			"applied":  *apply,
			"state_db": path,
			"matched":  plan.Matched,
			"swept":    plan.Swept,
			"missing":  plan.Missing,
			"total":    plan.Total(),
		})
	}

	printForgetPlan(os.Stdout, path, plan.Matched, plan.Swept, plan.Missing, *apply)
	return nil
}

func memoryShare(args []string) error {
	return runVisibility("memory share", args, lobslawv1.Visibility_VISIBILITY_SHARED)
}

func memoryUnshare(args []string) error {
	return runVisibility("memory unshare", args, lobslawv1.Visibility_VISIBILITY_PRIVATE)
}

// runVisibility is the shared body of share and unshare — the only
// difference between them is the target visibility.
func runVisibility(name string, args []string, to lobslawv1.Visibility) error {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var opts offlineStore
	opts.bind(fs)
	apply := fs.Bool("apply", false, "actually write; without it this is a dry run")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("at least one record id required")
	}

	store, path, err := opts.open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	changes, err := planVisibility(store, fs.Args(), to)
	if err != nil {
		return err
	}

	pending := 0
	for _, c := range changes {
		if c.From != c.To {
			pending++
		}
	}

	if *apply && pending > 0 {
		if err := applyVisibility(store, changes); err != nil {
			return err
		}
	}

	if *asJSON {
		out := make([]map[string]any, 0, len(changes))
		for _, c := range changes {
			out = append(out, map[string]any{
				"id":      c.ID,
				"kind":    c.Kind,
				"owner":   c.Owner,
				"from":    visLabel(c.From),
				"to":      visLabel(c.To),
				"changed": c.From != c.To,
			})
		}
		return emitJSON(map[string]any{
			"applied":  *apply,
			"state_db": path,
			"changes":  out,
			"pending":  pending,
		})
	}

	fmt.Printf("%s\n", path)
	for _, c := range changes {
		if c.From == c.To {
			fmt.Printf("  %-8s %s  owner=%s  already %s — no change\n", c.Kind, c.ID, c.Owner, visLabel(c.To))
			continue
		}
		fmt.Printf("  %-8s %s  owner=%s  %s → %s\n", c.Kind, c.ID, c.Owner, visLabel(c.From), visLabel(c.To))
	}
	switch {
	case pending == 0:
		fmt.Println("\nnothing to do — every record already has the requested visibility.")
	case *apply:
		fmt.Printf("\nUPDATED %d record(s).\n", pending)
	default:
		fmt.Printf("\nDRY RUN — nothing was written. Re-run with --apply to change %d record(s).\n", pending)
	}
	return nil
}

// visibilityChange is one record's before/after under share/unshare.
type visibilityChange struct {
	ID     string
	Kind   string
	Bucket string
	Owner  string
	From   lobslawv1.Visibility
	To     lobslawv1.Visibility
	rec    *memRecord
}

// planVisibility resolves ids into changes, refusing the whole batch
// if any id is unknown or unowned.
//
// All-or-nothing rather than skip-and-continue: an unowned record is
// an anomaly, not a normal state, and publishing part of a batch that
// contains one leaves the operator with a half-applied change to
// unpick on top of whatever produced the anomaly.
func planVisibility(store *memory.Store, ids []string, to lobslawv1.Visibility) ([]visibilityChange, error) {
	var (
		changes  []visibilityChange
		unknown  []string
		orphaned []string
	)
	for _, id := range ids {
		rec, err := loadMemRecord(store, id)
		if err != nil {
			return nil, err
		}
		if rec == nil {
			unknown = append(unknown, id)
			continue
		}
		if rec.owner() == "" {
			orphaned = append(orphaned, id)
			continue
		}
		changes = append(changes, visibilityChange{
			ID:     id,
			Kind:   rec.kind(),
			Bucket: rec.bucket,
			Owner:  rec.owner(),
			From:   rec.visibility(),
			To:     to,
			rec:    rec,
		})
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("no vector or episodic record with id: %s", strings.Join(unknown, ", "))
	}
	if len(orphaned) > 0 {
		return nil, fmt.Errorf("refusing to change visibility of unowned record(s): %s\n"+
			"an unowned record belongs to no principal, so there is nobody to share it on behalf of — "+
			"find out how it was written before publishing it", strings.Join(orphaned, ", "))
	}
	return changes, nil
}

// applyVisibility writes the flipped records back. Direct store writes,
// which is only sound because the node is stopped — there are no
// followers to diverge and no raft log to keep consistent.
func applyVisibility(store *memory.Store, changes []visibilityChange) error {
	for _, c := range changes {
		if c.From == c.To {
			continue
		}
		c.rec.setVisibility(c.To)
		raw, err := c.rec.marshal()
		if err != nil {
			return fmt.Errorf("marshal %s: %w", c.ID, err)
		}
		if err := store.Put(c.Bucket, c.ID, raw); err != nil {
			return fmt.Errorf("write %s/%s: %w", c.Bucket, c.ID, err)
		}
	}
	return nil
}

// memRecord is one memory record, whichever of the two buckets holds
// it. Exactly one of the pointers is non-nil.
type memRecord struct {
	bucket   string
	vector   *lobslawv1.VectorRecord
	episodic *lobslawv1.EpisodicRecord
}

// loadMemRecord finds id in either record bucket, returning (nil, nil)
// when it is in neither.
func loadMemRecord(store *memory.Store, id string) (*memRecord, error) {
	v, e, err := memory.FindRecord(store, id)
	switch {
	case err != nil:
		return nil, err
	case v != nil:
		return &memRecord{bucket: memory.BucketVectorRecords, vector: v}, nil
	case e != nil:
		return &memRecord{bucket: memory.BucketEpisodicRecords, episodic: e}, nil
	}
	return nil, nil
}

func (r *memRecord) kind() string {
	if r.vector != nil {
		return "vector"
	}
	return "episodic"
}

func (r *memRecord) id() string {
	if r.vector != nil {
		return r.vector.Id
	}
	return r.episodic.Id
}

func (r *memRecord) owner() string {
	if r.vector != nil {
		return r.vector.Owner
	}
	return r.episodic.Owner
}

func (r *memRecord) visibility() lobslawv1.Visibility {
	if r.vector != nil {
		return r.vector.Visibility
	}
	return r.episodic.Visibility
}

func (r *memRecord) setVisibility(v lobslawv1.Visibility) {
	if r.vector != nil {
		r.vector.Visibility = v
		return
	}
	r.episodic.Visibility = v
}

func (r *memRecord) marshal() ([]byte, error) {
	if r.vector != nil {
		return proto.Marshal(r.vector)
	}
	return proto.Marshal(r.episodic)
}

func (r *memRecord) fields() map[string]any {
	if r.vector != nil {
		return vectorFields(r.vector)
	}
	return episodicFields(r.episodic)
}

func (r *memRecord) fieldOrder() []string {
	if r.vector != nil {
		return []string{"id", "scope", "text", "metadata", "dims", "source_ids", "retention", "owner", "visibility", "created_at"}
	}
	return []string{"id", "event", "context", "importance", "tags", "retention", "source_ids", "owner", "visibility", "session_ref", "timestamp"}
}

// vectorFields is the full field set of a vector record, minus the raw
// embedding — a few thousand floats that no operator reads and that
// would bury every other field. Its length is reported as dims.
func vectorFields(v *lobslawv1.VectorRecord) map[string]any {
	return map[string]any{
		"id":         v.Id,
		"scope":      v.Scope,
		"text":       v.Text,
		"metadata":   v.Metadata,
		"dims":       len(v.Embedding),
		"source_ids": v.SourceIds,
		"retention":  retentionLabel(v.Retention),
		"owner":      v.Owner,
		"visibility": visLabel(v.Visibility),
		"created_at": tsString(v.CreatedAt),
	}
}

func episodicFields(e *lobslawv1.EpisodicRecord) map[string]any {
	return map[string]any{
		"id":          e.Id,
		"event":       e.Event,
		"context":     e.Context,
		"importance":  e.Importance,
		"tags":        e.Tags,
		"retention":   retentionLabel(e.Retention),
		"source_ids":  e.SourceIds,
		"owner":       e.Owner,
		"visibility":  visLabel(e.Visibility),
		"session_ref": e.SessionRef,
		"timestamp":   tsString(e.Timestamp),
	}
}

func vectorLine(v *lobslawv1.VectorRecord) string {
	return fmt.Sprintf("%s vector    %s  scope=%s dims=%d text=%dB sources=%v retention=%s owner=%s vis=%s created=%s",
		ownedMarker(v.Owner), v.Id, orNone(v.Scope), len(v.Embedding), len(v.Text), v.SourceIds,
		retentionLabel(v.Retention), orNone(v.Owner), visLabel(v.Visibility), orNone(tsString(v.CreatedAt)))
}

func episodicLine(e *lobslawv1.EpisodicRecord) string {
	line := fmt.Sprintf("%s episodic  %s  imp=%d tags=%v retention=%s sources=%v owner=%s vis=%s ts=%s",
		ownedMarker(e.Owner), e.Id, e.Importance, e.Tags, retentionLabel(e.Retention), e.SourceIds,
		orNone(e.Owner), visLabel(e.Visibility), orNone(tsString(e.Timestamp)))
	if e.SessionRef != "" {
		line += " session=" + e.SessionRef
	}
	if e.Event != "" {
		line += "\n             event: " + truncate(collapse(e.Event), 120)
	}
	return line
}

// referencedBy lists the consolidations that name id among their
// sources — exactly the set forget would cascade into.
func referencedBy(store *memory.Store, id string) ([]string, error) {
	return memory.ReferencedBy(store, id)
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	if v != "" {
		*s = append(*s, v)
	}
	return nil
}

// parseBefore accepts a full RFC3339 timestamp or a bare date. The
// bare date is the common case ("everything before last Tuesday") and
// resolves to midnight UTC.
func parseBefore(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--before %q: want RFC3339 (2026-01-02T15:04:05Z) or a date (2026-01-02)", s)
}

// sampleSize caps how many ids a dry run prints. Enough to recognise
// what is about to go, short enough to read.
const sampleSize = 10

func fprintSample(w io.Writer, ids []string) {
	for i, id := range ids {
		if i == sampleSize {
			_, _ = fmt.Fprintf(w, "             … and %d more\n", len(ids)-sampleSize)
			return
		}
		_, _ = fmt.Fprintf(w, "             %s\n", id)
	}
}

func emitJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// isEmptyField reports a field worth omitting from a `show`. Numbers
// are never omitted — a zero importance or a zero dimension count is
// information, and "this record has no embedding" is exactly the sort
// of thing an operator opens `show` to find out.
func isEmptyField(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case []string:
		return len(t) == 0
	case map[string]string:
		return len(t) == 0
	default:
		return false
	}
}

func ownedMarker(owner string) string {
	if owner == "" {
		return "!"
	}
	return " "
}

func visLabel(v lobslawv1.Visibility) string {
	return strings.TrimPrefix(v.String(), "VISIBILITY_")
}

func retentionLabel(r lobslawv1.Retention) string {
	return strings.TrimPrefix(r.String(), "RETENTION_")
}

func tsString(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return ""
	}
	return ts.AsTime().UTC().Format("2006-01-02 15:04:05 UTC")
}

// laterThan orders by timestamp, treating a missing one as oldest.
// Shared with session listing; the ordering rule itself lives in
// internal/memory so the scan and the renderers cannot disagree.
func laterThan(a, b *timestamppb.Timestamp) bool {
	return memory.LaterThan(a, b)
}

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shownOf(shown, total int) string {
	if shown == total {
		return fmt.Sprintf("%d", total)
	}
	return fmt.Sprintf("%d of %d", shown, total)
}

// collapse flattens whitespace so a multi-line field stays on one
// line in list output.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncate cuts at a rune boundary — byte slicing a UTF-8 string
// mid-rune produces mojibake in the middle of otherwise readable
// output.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
