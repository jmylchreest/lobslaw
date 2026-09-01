package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `memory list` and `memory show` opened state.db directly, which
// needs the node stopped — and on an operator's laptop there is no
// state.db to open. An empty listing then reads as an empty cluster.

// --- which way the command goes ----------------------------------------

// THE WIRING. Every subcommand with a live form goes live without
// --offline.
func TestEveryMemorySubcommandIsLiveByDefault(t *testing.T) {
	for sub, form := range memoryForms {
		got, liveMissing := memoryRoute(sub, false)
		if liveMissing {
			t.Errorf("%q reported no live form, but has one", sub)
		}
		if !routesTo(t, got, form.live) {
			t.Errorf("%q without --offline does not reach its live form", sub)
		}
		offline, _ := memoryRoute(sub, true)
		if !routesTo(t, offline, form.offline) {
			t.Errorf("%q with --offline does not reach its offline form", sub)
		}
	}
}

func TestTheMemoryFormsAreNotTheSame(t *testing.T) {
	for sub, form := range memoryForms {
		if routesTo(t, form.live, form.offline) {
			t.Errorf("%q has the same function for live and offline", sub)
		}
	}
}

// A subcommand with no live form still runs, and SAYS SO. Refusing a
// command that works to make a point about a flag would be worse —
// but running it silently is the failure R28 names.
func TestAnOfflineOnlySubcommandAnnouncesItself(t *testing.T) {
	for sub := range memoryOfflineOnly {
		fn, liveMissing := memoryRoute(sub, false)
		if fn == nil {
			t.Errorf("%q did not resolve at all", sub)
		}
		if !liveMissing {
			t.Errorf("%q ran without --offline and did not report that it has no live form", sub)
		}
		if _, stillMissing := memoryRoute(sub, true); stillMissing {
			t.Errorf("%q warned about a missing live form when --offline was passed", sub)
		}
	}
}

func TestAnUnknownMemorySubcommandHasNoRoute(t *testing.T) {
	if fn, _ := memoryRoute("lst", false); fn != nil {
		t.Error("a mistyped subcommand resolved to something")
	}
}

// Every routable subcommand must appear in the usage, or it is
// undiscoverable.
func TestEveryMemorySubcommandIsInTheUsage(t *testing.T) {
	for sub := range memoryForms {
		if !strings.Contains(memoryUsage, sub) {
			t.Errorf("usage does not mention %q", sub)
		}
	}
	for sub := range memoryOfflineOnly {
		if !strings.Contains(memoryUsage, sub) {
			t.Errorf("usage does not mention %q", sub)
		}
	}
	if !strings.Contains(memoryUsage, "--offline") {
		t.Error("usage does not mention --offline, so nobody finds the forensic path")
	}
}

// Without --offline the command must try to REACH A NODE rather than
// look for a local store.
func TestMemoryListGoesLiveByDefault(t *testing.T) {
	noAmbientCluster(t)

	err := memoryListLive(nil)
	if err == nil {
		t.Fatal("list with no connection details succeeded")
	}
	if strings.Contains(err.Error(), "state.db") {
		t.Errorf("the live path went looking for a local store: %v", err)
	}
}

// --- refusing before dialling ------------------------------------------

// A mistyped --kind costs no round trip, and must not silently mean
// "all" — that would show records the operator asked to exclude.
func TestAMistypedKindIsRefusedLocally(t *testing.T) {
	noAmbientCluster(t)

	err := memoryListLive([]string{"--kind", "vectors"})
	if err == nil {
		t.Fatal("an unknown --kind was accepted")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error %q does not name the flag", err)
	}
}

// An unfiltered forget matches every record in the store, and forget
// is irreversible. It must never leave the laptop.
func TestAnUnfilteredForgetNeverDials(t *testing.T) {
	noAmbientCluster(t)

	err := memoryForgetLive(nil)
	if err == nil {
		t.Fatal("an unfiltered forget was accepted")
	}
	if !strings.Contains(err.Error(), "refusing to forget everything") {
		t.Errorf("error %q does not explain the refusal", err)
	}
}

func TestShowNeedsExactlyOneId(t *testing.T) {
	noAmbientCluster(t)

	if err := memoryShowLive(nil); err == nil {
		t.Error("show with no id was accepted")
	}
	if err := memoryShowLive([]string{"a", "b"}); err == nil {
		t.Error("show with two ids was accepted")
	}
}

// THE FLAG THAT MUST NEVER INVERT. Forget cascades through SourceIds
// and is irreversible, so a "dry run" that deletes has no undo.
func TestApplyIsTheInverseOfDryRunForForget(t *testing.T) {
	withApply, err := forgetRequest([]string{"v1"}, "", "", nil, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if withApply.GetDryRun() {
		t.Error("--apply still asked for a dry run; nothing would be deleted")
	}

	without, err := forgetRequest([]string{"v1"}, "", "", nil, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !without.GetDryRun() {
		t.Fatal("no --apply still deleted; the default is meant to be a dry run")
	}
}

// Every filter must reach the request, or a narrow forget silently
// becomes a different one.
func TestEveryForgetFilterReachesTheRequest(t *testing.T) {
	req, err := forgetRequest([]string{"v1"}, "needle", "2026-01-02",
		[]string{"work"}, "user:alice", true)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case len(req.GetIds()) != 1:
		t.Errorf("ids = %v", req.GetIds())
	case req.GetQuery() != "needle":
		t.Errorf("query = %q", req.GetQuery())
	case req.GetBefore() == nil:
		t.Error("--before did not reach the request")
	case len(req.GetTags()) != 1:
		t.Errorf("tags = %v", req.GetTags())
	case req.GetRequester() != "user:alice":
		t.Errorf("requester = %q", req.GetRequester())
	}
}

// A --before that does not parse must say so rather than resolve to
// the zero time, which would mean "before the epoch" and match
// nothing.
func TestAnUnparseableBeforeIsRefused(t *testing.T) {
	if _, err := forgetRequest(nil, "", "last tuesday", nil, "", true); err == nil {
		t.Fatal("an unparseable --before was accepted")
	}
}

// --- what the answer says ----------------------------------------------

// The dry run an operator reads offline must be word-for-word the one
// they read live: this is the output somebody decides an irreversible
// delete on.
func TestTheForgetPlanWarnsAboutTheCascade(t *testing.T) {
	var buf bytes.Buffer
	printForgetPlan(&buf, "prod:9090", []string{"v1"}, []string{"summary"}, []string{"ghost"}, false)
	out := buf.String()

	for _, want := range []string{"prod:9090", "v1", "summary", "ghost", "DRY RUN"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "cascade") {
		t.Errorf("the cascade is not named:\n%s", out)
	}
	if strings.Contains(out, "DELETED") {
		t.Errorf("a dry run reports records as deleted:\n%s", out)
	}
}

func TestAnAppliedForgetSaysDeleted(t *testing.T) {
	var buf bytes.Buffer
	printForgetPlan(&buf, "prod:9090", []string{"v1"}, nil, nil, true)
	out := buf.String()
	if !strings.Contains(out, "DELETED") {
		t.Errorf("an applied forget is not announced:\n%s", out)
	}
	if strings.Contains(out, "DRY RUN") {
		t.Errorf("an applied forget still says dry run:\n%s", out)
	}
}

// A record with no owner is attributable to nobody, and share/unshare
// refuse to touch it — so the marker has to survive to the remote
// form as well.
func TestAnUnownedRecordIsFlagged(t *testing.T) {
	rec := &memRecord{
		bucket: memory.BucketVectorRecords,
		vector: &lobslawv1.VectorRecord{Id: "v1", Text: "hello"},
	}
	var buf bytes.Buffer
	printRecord(&buf, rec, nil)
	if !strings.Contains(buf.String(), unownedNote) {
		t.Errorf("an unowned record is not flagged:\n%s", buf.String())
	}
}

// What a forget would take with it, beside the record — finding that
// out afterwards is too late.
func TestShowNamesTheConsolidationsAForgetWouldSweep(t *testing.T) {
	rec := &memRecord{
		bucket: memory.BucketVectorRecords,
		vector: &lobslawv1.VectorRecord{Id: "v1", Owner: "user:alice", Text: "hello"},
	}
	var buf bytes.Buffer
	printRecord(&buf, rec, []string{"summary-1"})
	out := buf.String()
	if !strings.Contains(out, "summary-1") {
		t.Errorf("the referencing consolidation is missing:\n%s", out)
	}
	if !strings.Contains(out, "sweeps them too") {
		t.Errorf("the output does not say what would happen to it:\n%s", out)
	}
}

// The node must pick a kind. A success carrying neither record is a
// protocol mismatch, and saying "no such record" would send somebody
// hunting for a typo they did not make.
func TestAResponseWithNeitherRecordIsNotMistakenForAMiss(t *testing.T) {
	if rec := recordFromResponse(&lobslawv1.GetRecordResponse{}); rec != nil {
		t.Error("an empty response resolved to a record")
	}
	if rec := recordFromResponse(&lobslawv1.GetRecordResponse{
		Vector: &lobslawv1.VectorRecord{Id: "v1"},
	}); rec == nil || rec.kind() != "vector" {
		t.Error("a vector response did not resolve to a vector record")
	}
	if rec := recordFromResponse(&lobslawv1.GetRecordResponse{
		Episodic: &lobslawv1.EpisodicRecord{Id: "e1"},
	}); rec == nil || rec.kind() != "episodic" {
		t.Error("an episodic response did not resolve to an episodic record")
	}
}

// Every memory subcommand that reads or writes a bucket must have a
// live form.
//
// The bug this catches is not a missing function — it is
// `consolidations` quietly reading a laptop-local state.db, or
// refusing outright because the node it is asking about holds the
// lock. bbolt's lock is exclusive for the life of the process, so
// "offline only" means "unavailable while the thing it describes is
// running".
func TestEveryMemorySubcommandHasALiveForm(t *testing.T) {
	t.Parallel()
	for _, sub := range []string{"show", "list", "forget", "share", "unshare", "consolidations"} {
		form, ok := memoryForms[sub]
		if !ok {
			t.Errorf("memory %s has no live/offline pair", sub)
			continue
		}
		if form.live == nil {
			t.Errorf("memory %s has no live form", sub)
		}
	}
	if len(memoryOfflineOnly) != 0 {
		t.Errorf("memory subcommands still offline-only: %v", memoryOfflineOnly)
	}
}

// A write that stops partway must still say what it wrote.
//
// Returning a gRPC error discards the response, leaving the caller
// holding a partial change it cannot see — the worst possible answer
// for a command whose subject is who can read a memory.
func TestPartialVisibilityWriteIsReported(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	changes := []*lobslawv1.VisibilityChange{
		{Id: "a", Kind: "episodic", Owner: "user:john", Changed: true,
			From: lobslawv1.Visibility_VISIBILITY_PRIVATE, To: lobslawv1.Visibility_VISIBILITY_SHARED},
		{Id: "b", Kind: "episodic", Owner: "user:john", Changed: false,
			From: lobslawv1.Visibility_VISIBILITY_PRIVATE, To: lobslawv1.Visibility_VISIBILITY_SHARED},
	}
	if err := renderVisibilityChanges(&buf, changes, "node", true, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "UPDATED 1 record(s)") {
		t.Errorf("the count does not report what actually landed:\n%s", out)
	}
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") {
		t.Errorf("both records should be listed:\n%s", out)
	}
}
