package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `identity rebind` wrote straight to bbolt, so it needed the node
// stopped — and pointed at a follower's file while the cluster ran it
// would have written ownership no other replica has.

// --- which way the command goes ----------------------------------------

// THE WIRING. Without --offline the rewrite must REPLICATE.
func TestRebindIsLiveByDefault(t *testing.T) {
	for sub, form := range identityForms {
		if !routesTo(t, identityRoute(sub, false), form.live) {
			t.Errorf("%q without --offline does not reach its live form", sub)
		}
		if !routesTo(t, identityRoute(sub, true), form.offline) {
			t.Errorf("%q with --offline does not reach its offline form", sub)
		}
	}
}

func TestTheIdentityFormsAreNotTheSame(t *testing.T) {
	for sub, form := range identityForms {
		if routesTo(t, form.live, form.offline) {
			t.Errorf("%q has the same function for live and offline", sub)
		}
	}
}

func TestAnUnknownIdentitySubcommandHasNoRoute(t *testing.T) {
	if identityRoute("rebnd", false) != nil {
		t.Error("a mistyped subcommand resolved to something")
	}
}

func TestTheIdentityUsageMentionsBothPaths(t *testing.T) {
	for sub := range identityForms {
		if !strings.Contains(identityUsage, sub) {
			t.Errorf("usage does not mention %q", sub)
		}
	}
	if !strings.Contains(identityUsage, "--offline") {
		t.Error("usage does not mention --offline")
	}
	if !strings.Contains(identityUsage, "--apply") {
		t.Error("usage does not say the command is a dry run by default")
	}
}

// Without --offline the command must try to REACH A NODE rather than
// open a local store.
func TestRebindGoesLiveByDefault(t *testing.T) {
	noAmbientCluster(t)

	err := identityRebindLive([]string{"tg-@old", "tg-@new"})
	if err == nil {
		t.Fatal("rebind with no connection details succeeded")
	}
	if strings.Contains(err.Error(), "state.db") {
		t.Errorf("the live path went looking for a local store: %v", err)
	}
}

// --- refusing before dialling ------------------------------------------

func TestRebindNeedsTwoIds(t *testing.T) {
	for name, args := range map[string][]string{
		"none":  {},
		"one":   {"tg-@old"},
		"three": {"tg-@old", "tg-@new", "tg-@extra"},
	} {
		if _, _, err := rebindArgs(args); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Somebody who typed the same id twice meant something else, and
// running it would report a confident zero.
func TestRebindingAnIdToItselfNeverDials(t *testing.T) {
	if _, _, err := rebindArgs([]string{"tg-@a", "tg-@a"}); err == nil {
		t.Fatal("rebinding an id to itself was accepted")
	}
}

func TestRebindTrimsItsArguments(t *testing.T) {
	from, to, err := rebindArgs([]string{"  tg-@old  ", "tg-@new "})
	if err != nil {
		t.Fatal(err)
	}
	if from != "tg-@old" || to != "tg-@new" {
		t.Errorf("got %q -> %q", from, to)
	}
	// And whitespace alone is not an id.
	if _, _, err := rebindArgs([]string{"  ", "tg-@new"}); err == nil {
		t.Error("a blank <from> was accepted")
	}
}

// THE FLAG THAT MUST NEVER INVERT. A rebind rewrites ownership across
// seven buckets and there is no undo.
func TestApplyIsTheInverseOfDryRunForRebind(t *testing.T) {
	withApply, err := rebindRequest([]string{"tg-@old", "tg-@new"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if withApply.GetDryRun() {
		t.Error("--apply still asked for a dry run; nothing would move")
	}

	without, err := rebindRequest([]string{"tg-@old", "tg-@new"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !without.GetDryRun() {
		t.Fatal("no --apply still rewrote; the default is meant to be a dry run")
	}
	if without.GetFrom() != "tg-@old" || without.GetTo() != "tg-@new" {
		t.Errorf("request = %+v", without)
	}
}

// --- what the answer says ----------------------------------------------

func demoPlan() *memory.RebindPlan {
	return &memory.RebindPlan{
		Changes: map[string][]string{
			memory.BucketVectorRecords: {"v1", "v2"},
			memory.BucketSessions:      {"telegram:c1"},
		},
		Conflicts: []string{"user_prefs/tg-@old exists and is keyed by the id itself"},
	}
}

// A half-moved identity is worse than one that did not move, because
// nothing says which half — the SKIPPED lines are the only place that
// does.
func TestTheConflictsAreAlwaysPrinted(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRebind(&buf, demoPlan(), "tg-@old", "tg-@new", "prod:9090", false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "SKIPPED") || !strings.Contains(out, "user_prefs") {
		t.Errorf("the conflict is not reported:\n%s", out)
	}
}

// A dry run must not read as a completed migration.
func TestARebindDryRunSaysNothingWasWritten(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRebind(&buf, demoPlan(), "tg-@old", "tg-@new", "prod:9090", false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("a dry run does not announce itself:\n%s", out)
	}
	if strings.Contains(out, "REBOUND") {
		t.Errorf("a dry run reports records as moved:\n%s", out)
	}
	// The count is what somebody decides --apply on.
	if !strings.Contains(out, "3 record(s)") {
		t.Errorf("the total is missing:\n%s", out)
	}
}

func TestAnAppliedRebindSaysSo(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRebind(&buf, demoPlan(), "tg-@old", "tg-@new", "prod:9090", true, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "REBOUND 3") {
		t.Errorf("an applied rebind is not announced:\n%s", buf.String())
	}
}

// Which node, and which direction. A rebind report that says neither
// is unreadable a week later.
func TestTheRebindReportNamesItsSourceAndDirection(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRebind(&buf, demoPlan(), "tg-@old", "tg-@new", "prod:9090", false, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"prod:9090", "tg-@old -> tg-@new", memory.BucketVectorRecords} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
}

func TestNothingOwnedIsStated(t *testing.T) {
	var buf bytes.Buffer
	if err := renderRebind(&buf, &memory.RebindPlan{}, "tg-@ghost", "tg-@new", "prod:9090", false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "nothing owned by that id") {
		t.Errorf("an empty plan is not explained:\n%s", buf.String())
	}
}

// --- the wire round trip -----------------------------------------------

// The dry run an operator reads offline must be word-for-word the one
// they read live, so the response has to rebuild the same plan.
func TestTheResponseRebuildsThePlan(t *testing.T) {
	res := &lobslawv1.RebindResponse{
		Changes: []*lobslawv1.RebindBucketChange{
			{Bucket: memory.BucketVectorRecords, Ids: []string{"v1", "v2"}},
			{Bucket: memory.BucketSessions, Ids: []string{"telegram:c1"}},
		},
		Conflicts: []string{"user_prefs/tg-@old"},
		Applied:   3,
	}
	plan := planFromResponse(res)

	if plan.Total() != 3 {
		t.Errorf("total = %d, want 3", plan.Total())
	}
	if len(plan.Conflicts) != 1 {
		t.Errorf("conflicts = %v", plan.Conflicts)
	}
	// Buckets come back in a stable order, or the same rebind prints
	// differently on consecutive runs.
	got := plan.Buckets()
	if len(got) != 2 || got[0] > got[1] {
		t.Errorf("buckets = %v; not sorted", got)
	}
}
