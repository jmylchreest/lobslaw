package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The audit log is the record of what the agent was permitted to do.
// Reading it only from the local filesystem made it the record of what
// THIS MACHINE was permitted to do — which on an operator's laptop is
// nothing at all, and an empty audit log reads as a quiet cluster
// rather than as the wrong file.

// noAmbientCluster stops a developer's own config or contexts file
// from supplying a connection these tests are asserting the absence
// of. Without it the suite passes or fails depending on whose laptop
// it runs on.
func noAmbientCluster(t *testing.T) {
	t.Helper()
	t.Setenv("LOBSLAW_CONTEXTS", filepath.Join(t.TempDir(), "absent.toml"))
	t.Setenv("LOBSLAW_CONTEXT", "")
	t.Setenv("LOBSLAW_CONFIG", "")
	t.Setenv("LOBSLAW_NODE_ADDR", "")
	t.Setenv("LOBSLAW_AUDIT_PATH", "")
}

// --- which way the command goes ----------------------------------------

// routesTo names which implementation a subcommand actually reaches.
// Comparing the function itself, because the bug worth catching is not
// a missing function — it is `verify` quietly running the offline form
// and reporting a laptop's audit.jsonl as the cluster's.
func routesTo(t *testing.T, got, want func([]string) error) bool {
	t.Helper()
	if got == nil {
		return false
	}
	return reflect.ValueOf(got).Pointer() == reflect.ValueOf(want).Pointer()
}

// THE WIRING. Every subcommand goes LIVE without --offline.
func TestEveryAuditSubcommandIsLiveByDefault(t *testing.T) {
	for sub, form := range auditForms {
		if !routesTo(t, auditRoute(sub, false), form.live) {
			t.Errorf("%q without --offline does not reach its live form", sub)
		}
		if !routesTo(t, auditRoute(sub, true), form.offline) {
			t.Errorf("%q with --offline does not reach its offline form", sub)
		}
	}
}

// And the two forms are genuinely different functions, or the table
// above asserts nothing.
func TestTheLiveAndOfflineFormsAreNotTheSame(t *testing.T) {
	for sub, form := range auditForms {
		if routesTo(t, form.live, form.offline) {
			t.Errorf("%q has the same function for live and offline", sub)
		}
	}
}

func TestAnUnknownAuditSubcommandHasNoRoute(t *testing.T) {
	if auditRoute("verfy", false) != nil {
		t.Error("a mistyped subcommand resolved to something")
	}
}

// THE POINT. Without --offline, verify must try to REACH A NODE. The
// failure worth catching is a command that quietly reads a laptop-local
// audit.jsonl and reports the cluster is fine.
func TestVerifyGoesLiveByDefault(t *testing.T) {
	noAmbientCluster(t)

	err := auditVerifyLive(nil)
	if err == nil {
		t.Fatal("verify with no connection details succeeded")
	}
	if !strings.Contains(err.Error(), "--addr") && !strings.Contains(err.Error(), "--context") {
		t.Errorf("error %q does not say it could not reach a node", err)
	}
	// And specifically NOT a complaint about a local file, which would
	// mean it had gone looking for one.
	if strings.Contains(err.Error(), "audit.jsonl") {
		t.Errorf("the live path went looking for a local file: %v", err)
	}
}

func TestQueryGoesLiveByDefault(t *testing.T) {
	noAmbientCluster(t)

	err := auditQueryLive(nil)
	if err == nil {
		t.Fatal("query with no connection details succeeded")
	}
	if strings.Contains(err.Error(), "audit.jsonl") {
		t.Errorf("the live path went looking for a local file: %v", err)
	}
}

// The offline form is the forensic one and must not need a node.
func TestTheOfflineFormAsksForAFileNotANode(t *testing.T) {
	noAmbientCluster(t)

	err := auditVerifyOffline(nil)
	if err == nil {
		t.Fatal("offline verify with no path succeeded")
	}
	for _, want := range []string{"--path", "--config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

// --offline reaches the offline function, or the flag is decoration.
func TestTheOfflineFlagIsStrippedBeforeParsing(t *testing.T) {
	rest, offline := takeOffline([]string{"--offline", "--path", "/tmp/a.jsonl"})
	if !offline {
		t.Fatal("--offline was not recognised")
	}
	for _, a := range rest {
		if a == "--offline" {
			t.Fatal("--offline was left in the args and would fail the flag set")
		}
	}
}

// --- walking the sinks -------------------------------------------------

// fakeAudit answers VerifyChain from a script, one entry per sink.
type fakeAudit struct {
	lobslawv1.AuditServiceClient
	byS  map[string]*lobslawv1.VerifyChainResponse
	errs map[string]error
	saw  []string
}

func (f *fakeAudit) VerifyChain(_ context.Context, req *lobslawv1.VerifyChainRequest,
	_ ...grpc.CallOption) (*lobslawv1.VerifyChainResponse, error) {
	f.saw = append(f.saw, req.GetSink())
	if err := f.errs[req.GetSink()]; err != nil {
		return nil, err
	}
	return f.byS[req.GetSink()], nil
}

func testCtx() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// Each sink is walked SEPARATELY. The service flattens a no-sink
// verification into one ok/first_break, so an operator told "the chain
// is broken" would not be told which sink.
func TestEachSinkIsVerifiedSeparately(t *testing.T) {
	f := &fakeAudit{byS: map[string]*lobslawv1.VerifyChainResponse{
		"raft":  {Ok: true, EntriesChecked: 12},
		"local": {Ok: false, FirstBreakId: "e9", EntriesChecked: 4},
	}}

	results := verifySinks(f, testCtx, auditSinks)
	if len(f.saw) != 2 || f.saw[0] != "raft" || f.saw[1] != "local" {
		t.Fatalf("sinks asked: %v", f.saw)
	}
	var buf bytes.Buffer
	broken, checked := renderSinkResults(&buf, "prod:9090", results)
	if !broken || !checked {
		t.Fatalf("broken=%v checked=%v; a broken sink was not reported", broken, checked)
	}
	out := buf.String()
	if !strings.Contains(out, "local") || !strings.Contains(out, "e9") {
		t.Errorf("output does not name the broken sink or entry:\n%s", out)
	}
	if !strings.Contains(out, "raft") || !strings.Contains(out, "12") {
		t.Errorf("the healthy sink is missing from the report:\n%s", out)
	}
}

// A sink this node does not run is not a broken chain. Conflating the
// two sends somebody looking for tampering that did not happen.
func TestAnUnavailableSinkIsNotABrokenChain(t *testing.T) {
	f := &fakeAudit{
		byS:  map[string]*lobslawv1.VerifyChainResponse{"local": {Ok: true, EntriesChecked: 3}},
		errs: map[string]error{"raft": errors.New("rpc error: code = InvalidArgument desc = verify: unknown sink")},
	}

	results := verifySinks(f, testCtx, auditSinks)
	var buf bytes.Buffer
	broken, checked := renderSinkResults(&buf, "prod:9090", results)
	if broken {
		t.Error("an unavailable sink was reported as a broken chain")
	}
	if !checked {
		t.Error("the sink that did answer was not counted as checked")
	}
	if !strings.Contains(buf.String(), "not available") {
		t.Errorf("the unavailable sink is not explained:\n%s", buf.String())
	}
}

// One failing sink must not stop the others, or a sink the node does
// not run hides a real break in the one it does.
func TestAFailingSinkDoesNotStopTheWalk(t *testing.T) {
	f := &fakeAudit{
		byS:  map[string]*lobslawv1.VerifyChainResponse{"local": {Ok: false, FirstBreakId: "e1"}},
		errs: map[string]error{"raft": errors.New("boom")},
	}

	results := verifySinks(f, testCtx, auditSinks)
	if len(results) != 2 {
		t.Fatalf("got %d results; the walk stopped early", len(results))
	}
	var buf bytes.Buffer
	if broken, _ := renderSinkResults(&buf, "prod:9090", results); !broken {
		t.Error("a break behind a failing sink went unreported")
	}
}

// THE R28 RULE. Exit 0 having verified nothing is the failure this
// command exists to catch: a confident answer about a record nobody
// read.
func TestNothingVerifiedIsNotAPass(t *testing.T) {
	f := &fakeAudit{errs: map[string]error{
		"raft": errors.New("unavailable"), "local": errors.New("unavailable"),
	}}

	var buf bytes.Buffer
	broken, checked := renderSinkResults(&buf, "prod:9090", verifySinks(f, testCtx, auditSinks))
	if checked {
		t.Fatal("nothing answered, yet the command counted a sink as checked")
	}
	if broken {
		t.Error("nothing answered, yet the chain was reported broken")
	}
}

// The verdict must say which node produced it. "OK" about an
// unspecified cluster is the same problem in a different place.
func TestTheVerdictNamesItsSource(t *testing.T) {
	f := &fakeAudit{byS: map[string]*lobslawv1.VerifyChainResponse{"raft": {Ok: true}}}
	var buf bytes.Buffer
	renderSinkResults(&buf, "prod.example:9090", verifySinks(f, testCtx, []string{"raft"}))
	if !strings.Contains(buf.String(), "prod.example:9090") {
		t.Errorf("the verdict does not say which node it is about:\n%s", buf.String())
	}
}

// --- reading the window an operator typed ------------------------------

// "--since 24h" is what somebody types. Making them work out this
// morning's timestamp in RFC3339 is how a filter goes unused.
func TestADurationIsReadAsAgo(t *testing.T) {
	got, err := parseWhen("24h")
	if err != nil {
		t.Fatal(err)
	}
	ago := time.Since(got)
	if ago < 23*time.Hour || ago > 25*time.Hour {
		t.Errorf("--since 24h resolved to %v ago", ago)
	}
}

// A negative duration means the same thing somebody meant by "24h".
// Reading it forward would silently produce a window in the future.
func TestANegativeDurationStillMeansAgo(t *testing.T) {
	got, err := parseWhen("-24h")
	if err != nil {
		t.Fatal(err)
	}
	if got.After(time.Now()) {
		t.Errorf("-24h resolved to %v, which is in the future", got)
	}
}

func TestAnRFC3339InstantIsTakenLiterally(t *testing.T) {
	got, err := parseWhen("2026-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	if got.UTC().Format(time.RFC3339) != "2026-01-02T03:04:05Z" {
		t.Errorf("got %v", got)
	}
}

func TestAnEmptyWindowIsNotAnError(t *testing.T) {
	got, err := parseWhen("")
	if err != nil || !got.IsZero() {
		t.Errorf("got %v, %v; an unset filter is not an error", got, err)
	}
}

// A typo must say so. Silently treating it as "no filter" would return
// the whole log and look like the filter matched everything.
func TestAnUnparseableWindowIsRefused(t *testing.T) {
	if _, err := parseWhen("last tuesday"); err == nil {
		t.Fatal("an unparseable --since was accepted")
	}
}

// An empty result from a backwards window looks exactly like an empty
// result from a quiet cluster, and only one of them is a question the
// operator meant to ask.
func TestABackwardsWindowIsRefused(t *testing.T) {
	f := auditFilterFlags{since: "2026-01-02T00:00:00Z", until: "2026-01-01T00:00:00Z"}
	_, err := f.resolve()
	if err == nil {
		t.Fatal("a window ending before it starts was accepted")
	}
	if !strings.Contains(err.Error(), "before") {
		t.Errorf("error %q does not explain the window", err)
	}
}

func TestAForwardWindowIsAccepted(t *testing.T) {
	f := auditFilterFlags{since: "2026-01-01T00:00:00Z", until: "2026-01-02T00:00:00Z", limit: 10}
	got, err := f.resolve()
	if err != nil {
		t.Fatalf("a valid window was refused: %v", err)
	}
	if got.Limit != 10 {
		t.Errorf("limit = %d", got.Limit)
	}
}

// The filters must actually reach the filter, or every query returns
// the whole log and looks like a very busy cluster.
func TestTheFiltersReachTheQuery(t *testing.T) {
	f := auditFilterFlags{actor: "user:alice", action: "tool:exec", target: "/etc/passwd"}
	got, err := f.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got.ActorScope != "user:alice" || got.Action != "tool:exec" || got.Target != "/etc/passwd" {
		t.Errorf("filter = %+v", got)
	}
}

// --- saying where the answer came from ---------------------------------

// An empty audit log is indistinguishable from the wrong file unless
// the source is on the page. That is the whole failure mode R28 names.
func TestAnEmptyResultStillNamesItsSource(t *testing.T) {
	var buf bytes.Buffer
	if err := renderAuditEntries(&buf, nil, "prod.example:9090", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "prod.example:9090") {
		t.Errorf("an empty result does not say where it looked:\n%s", out)
	}
}

func TestEntriesAreRenderedWithTheirEffect(t *testing.T) {
	entries := []types.AuditEntry{{
		ID:         "a1",
		Timestamp:  time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ActorScope: "user:alice",
		Action:     "tool:exec",
		Target:     "/bin/ls",
		Effect:     types.Effect("allow"),
		PolicyRule: "rule-7",
	}}
	var buf bytes.Buffer
	if err := renderAuditEntries(&buf, entries, "/var/log/audit.jsonl", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"user:alice", "tool:exec", "/bin/ls", "allow", "rule-7", "/var/log/audit.jsonl"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// --- locating the offline file -----------------------------------------

func TestADisabledLocalSinkSaysThereIsNoOfflineCopy(t *testing.T) {
	noAmbientCluster(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("[audit.local]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := openLocalAudit("", cfg)
	if err == nil {
		t.Fatal("a disabled local sink resolved to a file")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error %q does not say the sink is off", err)
	}
}

func TestAnExplicitPathBeatsTheConfig(t *testing.T) {
	noAmbientCluster(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfg, []byte("[audit.local]\nenabled = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sink, resolved, err := openLocalAudit(path, cfg)
	if err != nil {
		t.Fatalf("--path did not override a config that disables the sink: %v", err)
	}
	defer func() { _ = sink.Close() }()
	if resolved != path {
		t.Errorf("resolved = %q, want %q", resolved, path)
	}
}

// A missing file must say so rather than read as an empty log.
func TestAMissingAuditFileIsAnError(t *testing.T) {
	noAmbientCluster(t)
	if _, _, err := openLocalAudit(filepath.Join(t.TempDir(), "gone.jsonl"), ""); err == nil {
		t.Fatal("a missing audit.jsonl opened as an empty log")
	}
}

// --- the usage -----------------------------------------------------------

func TestEveryAuditSubcommandIsInTheUsage(t *testing.T) {
	for _, name := range []string{"verify", "query"} {
		if !strings.Contains(auditUsage, name) {
			t.Errorf("usage does not mention %q", name)
		}
	}
	if !strings.Contains(auditUsage, "--offline") {
		t.Error("usage does not mention --offline, so nobody finds the forensic path")
	}
}

// The gRPC prefix is three quarters of the line and none of the answer.
func TestAStatusErrorIsTrimmedToItsMessage(t *testing.T) {
	got := auditShortErr("rpc error: code = InvalidArgument desc = verify: unknown sink \"raft\"")
	if strings.Contains(got, "rpc error") {
		t.Errorf("got %q", got)
	}
	if !strings.Contains(got, "unknown sink") {
		t.Errorf("got %q; the message was lost", got)
	}
}
