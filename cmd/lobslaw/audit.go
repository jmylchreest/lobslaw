package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/pkg/config"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The audit log is the record of what the agent was permitted to do.
// Reading it only from the local filesystem made it the record of what
// THIS MACHINE was permitted to do, which on an operator's laptop is
// nothing at all — and an empty audit log reads as a quiet cluster
// rather than as the wrong file.
//
// So live is the default and --offline is the opt-out. The offline
// path stays because it is the forensic one: a node that will not
// start still has its audit.jsonl, and that is exactly when somebody
// wants to read it.

const auditUsage = `lobslaw audit — the tamper-evident record of what was permitted

subcommands:
  verify    walk the hash chain and report breaks
  query     read entries, newest first

Both talk to a RUNNING node over mTLS by default — use --context, or
--addr with the credential flags. Pass --offline to read the local
audit.jsonl directly instead; that path needs no node and is the one
for a cluster that will not start.

--sink selects raft or local on a running node. Omitted, verify checks
every sink SEPARATELY and names any that breaks.`

func dispatchAudit(args []string) bool {
	idx := findSubcmd(args, "audit")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, auditUsage)
		os.Exit(2)
	}

	rest, offline := takeOffline(sub[1:])

	run := auditRoute(sub[0], offline)
	if run == nil {
		fmt.Fprintf(os.Stderr, "unknown audit subcommand %q\n\n%s\n", sub[0], auditUsage)
		os.Exit(2)
	}
	err := run(rest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit %s: %v\n", sub[0], err)
		os.Exit(1)
	}
	return true
}

// auditForms pairs each subcommand's live and offline implementation.
//
// A table rather than a switch so the ROUTING is a value a test can
// assert. The bug worth catching is not a missing function — it is
// `verify` quietly running the offline form and reporting a laptop's
// audit.jsonl as the cluster's.
var auditForms = map[string]struct{ live, offline func([]string) error }{
	"verify": {live: auditVerifyLive, offline: auditVerifyOffline},
	"query":  {live: auditQueryLive, offline: auditQueryOffline},
}

// auditRoute returns the implementation for a subcommand, or nil if
// there is none. Live is the default; --offline is the opt-out.
func auditRoute(sub string, offline bool) func([]string) error {
	form, ok := auditForms[sub]
	if !ok {
		return nil
	}
	if offline {
		return form.offline
	}
	return form.live
}

func auditClient(node *liveNode) (lobslawv1.AuditServiceClient, func(), error) {
	conn, err := node.dial()
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewAuditServiceClient(conn), func() { _ = conn.Close() }, nil
}

// --- verify ------------------------------------------------------------

// auditSinks are the sinks verify walks when none was named.
//
// Checked SEPARATELY rather than in one call, because the service
// flattens a no-sink verification into a single ok/first_break and an
// operator told "the chain is broken" without being told which sink
// has half an answer. Its own doc says to call per sink for detail;
// this is the caller that does.
var auditSinks = []string{"raft", "local"}

func auditVerifyLive(args []string) error {
	fs := flag.NewFlagSet("audit verify", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	sink := fs.String("sink", "", "raft or local; default checks each separately")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, closeConn, err := auditClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	wanted := auditSinks
	if *sink != "" {
		wanted = []string{*sink}
	}

	results := verifySinks(client, node.ctx, wanted)
	if *asJSON {
		return emitJSON(map[string]any{"source": node.addr, "results": results})
	}

	broken, checkedAny := renderSinkResults(os.Stdout, node.addr, results)
	switch {
	case broken:
		// Returned rather than exiting here, so the connection closes
		// and the exit code still comes out 1 via the dispatcher. A
		// broken chain must not be a zero exit: something scripted
		// around this command would read silence as health.
		return errors.New("the audit chain is broken; see above")
	case !checkedAny:
		// Every sink refused. Reporting "audit OK" here would be the
		// exact failure this command exists to catch: a confident
		// answer about a record nobody read.
		return fmt.Errorf("no sink could be verified on %s", node.addr)
	}
	return nil
}

// sinkResult is one sink's verdict, or why it produced none.
type sinkResult struct {
	Sink    string `json:"sink"`
	OK      bool   `json:"ok"`
	Checked int64  `json:"entries_checked"`
	BreakID string `json:"first_break_id,omitempty"`
	// Err is set when the sink did not answer. A sink the node does not
	// have is NOT a broken chain, and conflating the two would send
	// somebody looking for tampering that did not happen.
	Err string `json:"error,omitempty"`
}

// answered reports whether this sink was actually walked. Distinct
// from OK: a sink that errored has OK=false and proves nothing.
func (r sinkResult) answered() bool { return r.Err == "" }

// verifySinks walks each sink separately.
//
// One call per sink rather than one call for all of them, because the
// service flattens a no-sink verification into a single ok/first_break
// and an operator told "the chain is broken" without being told which
// sink has half an answer. The service's own doc says to call per sink
// for detail; this is the caller that does.
//
// A sink that errors does not stop the others: the likeliest error is
// a sink this node does not run, and that must not hide a real break
// in the one it does.
func verifySinks(client lobslawv1.AuditServiceClient, newCtx func() (context.Context, context.CancelFunc), sinks []string) []sinkResult {
	out := make([]sinkResult, 0, len(sinks))
	for _, name := range sinks {
		ctx, cancel := newCtx()
		res, err := client.VerifyChain(ctx, &lobslawv1.VerifyChainRequest{Sink: name})
		cancel()
		if err != nil {
			out = append(out, sinkResult{Sink: name, Err: err.Error()})
			continue
		}
		out = append(out, sinkResult{
			Sink:    name,
			OK:      res.GetOk(),
			Checked: res.GetEntriesChecked(),
			BreakID: res.GetFirstBreakId(),
		})
	}
	return out
}

// renderSinkResults prints the verdicts and reports whether anything
// broke and whether anything was actually checked.
//
// The second return is the one that matters: exit 0 having verified
// nothing is the failure R28 names, so the caller refuses rather than
// reporting a clean chain.
func renderSinkResults(w io.Writer, source string, results []sinkResult) (broken, checkedAny bool) {
	_, _ = fmt.Fprintf(w, "%s\n", source)
	for _, r := range results {
		switch {
		case !r.answered():
			_, _ = fmt.Fprintf(w, "  %-6s not available (%s)\n", r.Sink, auditShortErr(r.Err))
		case r.OK:
			checkedAny = true
			_, _ = fmt.Fprintf(w, "  %-6s OK — %d entries\n", r.Sink, r.Checked)
		default:
			checkedAny = true
			broken = true
			_, _ = fmt.Fprintf(w, "  %-6s BROKEN at %s (after %d entries)\n", r.Sink, r.BreakID, r.Checked)
		}
	}
	return broken, checkedAny
}

func auditVerifyOffline(args []string) error {
	fs := flag.NewFlagSet("audit verify --offline", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""), "path to config.toml (used to locate the local audit file)")
	path := fs.String("path", envOr("LOBSLAW_AUDIT_PATH", ""), "explicit path to audit.jsonl; overrides --config")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sink, resolved, err := openLocalAudit(*path, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close() }()

	res, err := sink.VerifyChain(context.Background())
	if err != nil {
		return fmt.Errorf("verify chain: %w", err)
	}

	abs, _ := filepath.Abs(resolved)
	if *asJSON {
		return emitJSON(map[string]any{
			"sink": "local", "path": abs, "ok": res.OK,
			"entries_checked": res.EntriesChecked, "first_break_id": res.FirstBreakID,
		})
	}
	if res.OK {
		fmt.Printf("audit OK: %d entries checked at %s\n", res.EntriesChecked, abs)
		return nil
	}
	fmt.Fprintf(os.Stderr, "audit BROKEN at entry %s (after %d entries) — %s\n",
		res.FirstBreakID, res.EntriesChecked, abs)
	// gocritic exitAfterDefer: the deferred sink.Close is knowingly
	// skipped. `audit verify` never appends, and LocalSink's
	// lumberjack writer opens the file lazily on first write, so
	// there is no handle to flush and nothing to lose.
	os.Exit(1) //nolint:gocritic // exitAfterDefer: sink.Close is a no-op on this read-only path
	return nil
}

// --- query -------------------------------------------------------------

// auditFilterFlags are shared by the live and offline forms so the two
// cannot drift into accepting different questions.
type auditFilterFlags struct {
	actor  string
	action string
	target string
	since  string
	until  string
	limit  int
}

func (f *auditFilterFlags) bind(fs *flag.FlagSet) {
	fs.StringVar(&f.actor, "actor", "", "match actor scope exactly")
	fs.StringVar(&f.action, "action", "", "match action exactly")
	fs.StringVar(&f.target, "target", "", "match target exactly")
	fs.StringVar(&f.since, "since", "", "RFC3339 timestamp, or a duration like 24h")
	fs.StringVar(&f.until, "until", "", "RFC3339 timestamp, or a duration like 1h")
	fs.IntVar(&f.limit, "limit", 50, "maximum entries (0 for no limit)")
}

func (f *auditFilterFlags) resolve() (types.AuditFilter, error) {
	out := types.AuditFilter{
		ActorScope: f.actor,
		Action:     f.action,
		Target:     f.target,
		Limit:      f.limit,
	}
	var err error
	if out.Since, err = parseWhen(f.since); err != nil {
		return out, fmt.Errorf("--since: %w", err)
	}
	if out.Until, err = parseWhen(f.until); err != nil {
		return out, fmt.Errorf("--until: %w", err)
	}
	if !out.Since.IsZero() && !out.Until.IsZero() && out.Until.Before(out.Since) {
		// An empty result from a backwards window looks exactly like an
		// empty result from a quiet cluster, and only one of them is a
		// question the operator meant to ask.
		return out, fmt.Errorf("--until %s is before --since %s; that window contains nothing",
			out.Until.Format(time.RFC3339), out.Since.Format(time.RFC3339))
	}
	return out, nil
}

// parseWhen accepts an RFC3339 instant or a bare duration meaning "ago".
//
// "--since 24h" is what somebody types; making them work out this
// morning's timestamp in RFC3339 is how a filter goes unused.
func parseWhen(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither an RFC3339 time nor a duration like 24h", v)
	}
	if d < 0 {
		d = -d
	}
	return time.Now().Add(-d), nil
}

func auditQueryLive(args []string) error {
	fs := flag.NewFlagSet("audit query", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	var filter auditFilterFlags
	filter.bind(fs)
	sink := fs.String("sink", "", "raft or local; default is every sink the node has")
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	f, err := filter.resolve()
	if err != nil {
		return err
	}

	client, closeConn, err := auditClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	req := &lobslawv1.QueryRequest{
		ActorScope: f.ActorScope,
		Action:     f.Action,
		Target:     f.Target,
		Limit:      int32(f.Limit), //nolint:gosec // a CLI --limit is not attacker-controlled
		Sink:       *sink,
	}
	if !f.Since.IsZero() {
		req.Since = timestamppb.New(f.Since)
	}
	if !f.Until.IsZero() {
		req.Until = timestamppb.New(f.Until)
	}

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.Query(ctx, req)
	if err != nil {
		return err
	}

	entries := make([]types.AuditEntry, 0, len(res.GetEntries()))
	for _, e := range res.GetEntries() {
		entries = append(entries, auditProtoToTyped(e))
	}
	return renderAuditEntries(os.Stdout, entries, node.addr, *asJSON)
}

func auditQueryOffline(args []string) error {
	fs := flag.NewFlagSet("audit query --offline", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("LOBSLAW_CONFIG", ""), "path to config.toml (used to locate the local audit file)")
	path := fs.String("path", envOr("LOBSLAW_AUDIT_PATH", ""), "explicit path to audit.jsonl; overrides --config")
	var filter auditFilterFlags
	filter.bind(fs)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	f, err := filter.resolve()
	if err != nil {
		return err
	}

	sink, resolved, err := openLocalAudit(*path, *cfgPath)
	if err != nil {
		return err
	}
	defer func() { _ = sink.Close() }()

	entries, err := sink.Query(context.Background(), f)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	abs, _ := filepath.Abs(resolved)
	return renderAuditEntries(os.Stdout, entries, abs, *asJSON)
}

// --- shared ------------------------------------------------------------

// openLocalAudit resolves and opens the local audit.jsonl.
//
// The error says which of --path and --config it wanted rather than
// "configuration error": on a laptop exactly one of them is missing,
// and which one is the whole answer.
func openLocalAudit(path, cfgPath string) (*audit.LocalSink, string, error) {
	resolved := path
	if resolved == "" {
		if cfgPath == "" {
			return nil, "", fmt.Errorf("--offline needs either --path or --config to find audit.jsonl")
		}
		cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
		if err != nil {
			return nil, "", fmt.Errorf("load config: %w", err)
		}
		if !cfg.Audit.Local.Enabled {
			return nil, "", fmt.Errorf("the local sink is disabled in %s; there is no offline copy to read", cfgPath)
		}
		resolved = cfg.Audit.Local.Path
		if resolved == "" {
			return nil, "", fmt.Errorf("[audit.local].path is empty in %s", cfgPath)
		}
	}
	if _, err := os.Stat(resolved); err != nil {
		return nil, "", err
	}
	sink, err := audit.NewLocalSink(audit.LocalConfig{Path: resolved})
	if err != nil {
		return nil, "", fmt.Errorf("open audit log %q: %w", resolved, err)
	}
	return sink, resolved, nil
}

// renderAuditEntries prints entries and, crucially, SAYS WHERE THEY
// CAME FROM. An empty audit log is indistinguishable from the wrong
// file unless the source is on the page.
func renderAuditEntries(w io.Writer, entries []types.AuditEntry, source string, asJSON bool) error {
	if asJSON {
		return emitJSON(map[string]any{"source": source, "entries": entries})
	}
	_, _ = fmt.Fprintf(w, "%s\n", source)
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "no entries matched.")
		return nil
	}
	for _, e := range entries {
		_, _ = fmt.Fprintf(w, "  %s  %-16s %-24s %s\n",
			e.Timestamp.Format("2006-01-02 15:04:05"),
			truncate(e.ActorScope, 16), truncate(e.Action, 24), e.Target)
		if e.Effect != "" || e.PolicyRule != "" {
			_, _ = fmt.Fprintf(w, "      effect=%s rule=%s\n", e.Effect, e.PolicyRule)
		}
	}
	_, _ = fmt.Fprintf(w, "\n%d entr%s.\n", len(entries), plural(len(entries)))
	return nil
}

func auditProtoToTyped(e *lobslawv1.AuditEntry) types.AuditEntry {
	out := types.AuditEntry{
		ID:         e.GetId(),
		ActorScope: e.GetActorScope(),
		Action:     e.GetAction(),
		Target:     e.GetTarget(),
		Argv:       e.GetArgv(),
		PolicyRule: e.GetPolicyRule(),
		Effect:     types.Effect(e.GetEffect()),
		ResultHash: e.GetResultHash(),
		PrevHash:   e.GetPrevHash(),
	}
	if e.GetTimestamp() != nil {
		out.Timestamp = e.GetTimestamp().AsTime()
	}
	return out
}

// auditShortErr trims a gRPC status down to its message. The full
// "rpc error: code = ..." prefix is three quarters of the line and
// none of the answer.
func auditShortErr(err string) string {
	if i := strings.LastIndex(err, "desc = "); i >= 0 {
		return err[i+len("desc = "):]
	}
	return err
}
