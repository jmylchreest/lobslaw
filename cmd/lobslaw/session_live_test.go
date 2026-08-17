package main

import (
	"bytes"
	"strings"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `session list` printed "Total sessions: 0" on any machine that is
// not the node, because there was no state.db to open — and a count of
// zero reads as a quiet cluster rather than as the wrong store.

// --- which way the command goes ----------------------------------------

// THE WIRING. Every subcommand goes LIVE without --offline.
func TestEverySessionSubcommandIsLiveByDefault(t *testing.T) {
	for sub, form := range sessionForms {
		if !routesTo(t, sessionRoute(sub, false), form.live) {
			t.Errorf("%q without --offline does not reach its live form", sub)
		}
		if !routesTo(t, sessionRoute(sub, true), form.offline) {
			t.Errorf("%q with --offline does not reach its offline form", sub)
		}
	}
}

func TestTheSessionFormsAreNotTheSame(t *testing.T) {
	for sub, form := range sessionForms {
		if routesTo(t, form.live, form.offline) {
			t.Errorf("%q has the same function for live and offline", sub)
		}
	}
}

func TestAnUnknownSessionSubcommandHasNoRoute(t *testing.T) {
	if sessionRoute("lst", false) != nil {
		t.Error("a mistyped subcommand resolved to something")
	}
}

func TestEverySessionSubcommandIsInTheUsage(t *testing.T) {
	for sub := range sessionForms {
		if !strings.Contains(sessionUsage, sub) {
			t.Errorf("usage does not mention %q", sub)
		}
	}
	if !strings.Contains(sessionUsage, "--offline") {
		t.Error("usage does not mention --offline, so nobody finds the forensic path")
	}
}

// Without --offline the command must try to REACH A NODE rather than
// look for a local store.
func TestSessionListGoesLiveByDefault(t *testing.T) {
	noAmbientCluster(t)

	err := sessionListLive(nil)
	if err == nil {
		t.Fatal("list with no connection details succeeded")
	}
	if strings.Contains(err.Error(), "state.db") {
		t.Errorf("the live path went looking for a local store: %v", err)
	}
}

// --- refusing before dialling ------------------------------------------

// Enumeration is `session list`. An empty search matching every
// conversation would read as "they all mention it".
func TestAnEmptySearchNeverDials(t *testing.T) {
	noAmbientCluster(t)

	err := sessionSearchLive(nil)
	if err == nil {
		t.Fatal("a search with no text was accepted")
	}
	// Matched on the whole phrase: "text" alone also appears inside
	// "--context", so the loose check passed even when the guard was
	// removed and the command went on to dial.
	if !strings.Contains(err.Error(), "search text required") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

func TestSessionShowNeedsExactlyOneId(t *testing.T) {
	noAmbientCluster(t)

	if err := sessionShowLive(nil); err == nil {
		t.Error("show with no id was accepted")
	}
	if err := sessionShowLive([]string{"a", "b"}); err == nil {
		t.Error("show with two ids was accepted")
	}
}

// --- saying where the answer came from ---------------------------------

// "Total sessions: 0" without a source is the exact failure R28 names.
func TestAnEmptySessionListNamesItsSource(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSessionList(&buf, nil, "prod.example:9090", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "prod.example:9090") {
		t.Errorf("an empty list does not say where it looked:\n%s", out)
	}
	if !strings.Contains(out, "Total sessions: 0") {
		t.Errorf("the count is missing:\n%s", out)
	}
}

func TestTheListingShowsTheRunningSummary(t *testing.T) {
	rec := &lobslawv1.SessionRecord{
		Id: "telegram:c1", Channel: "telegram", UserId: "alice",
		Title: "about pelicans", Summary: "they discussed pelicans",
		SummaryThroughSeq: 7, FirstSeq: 1, NextSeq: 9,
	}
	var buf bytes.Buffer
	if err := renderSessionList(&buf, []*lobslawv1.SessionRecord{rec}, "prod:9090", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"telegram:c1", "alice", "about pelicans", "they discussed pelicans"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
}

func TestTheTranscriptNamesItsSource(t *testing.T) {
	rec := &lobslawv1.SessionRecord{Id: "telegram:c1", Channel: "telegram", FirstSeq: 1, NextSeq: 2}
	msgs := []*lobslawv1.SessionMessage{{Seq: 1, Role: "user", Content: "hello there"}}

	var buf bytes.Buffer
	if err := renderTranscript(&buf, rec, msgs, "prod:9090", 0, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "prod:9090") {
		t.Errorf("the transcript does not say which node it came from:\n%s", out)
	}
	if !strings.Contains(out, "hello there") {
		t.Errorf("the message is missing:\n%s", out)
	}
}

// --truncate must actually shorten, or a transcript full of tool
// results is unreadable and the flag is decoration.
func TestTruncateShortensTheMessages(t *testing.T) {
	rec := &lobslawv1.SessionRecord{Id: "telegram:c1"}
	msgs := []*lobslawv1.SessionMessage{
		{Seq: 1, Role: "user", Content: strings.Repeat("x", 500)},
	}
	var buf bytes.Buffer
	if err := renderTranscript(&buf, rec, msgs, "prod:9090", 20, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), strings.Repeat("x", 100)) {
		t.Errorf("--truncate did not shorten the message:\n%s", buf.String())
	}
}

// The total match count, not the snippet count: it is what tells a
// passing mention from a thread about the thing.
func TestSearchOutputShowsTheTotalMatchCount(t *testing.T) {
	hits := []*lobslawv1.SessionSearchHitProto{{
		Session: &lobslawv1.SessionRecord{Id: "telegram:c1", Title: "pelicans"},
		Matches: 12,
		Snippets: []*lobslawv1.SessionSnippetProto{
			{Seq: 3, Role: "user", Text: "the pelican brief"},
		},
	}}
	var buf bytes.Buffer
	if err := renderSessionHits(&buf, hits, "pelican", "prod:9090", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "12 match") {
		t.Errorf("the total match count is missing:\n%s", out)
	}
	if !strings.Contains(out, "the pelican brief") {
		t.Errorf("the snippet is missing:\n%s", out)
	}
	if !strings.Contains(out, "prod:9090") {
		t.Errorf("the results do not say which node they came from:\n%s", out)
	}
}

// A conversation with no title still needs to be identifiable, or the
// hit is a blank line with an id.
func TestAnUntitledConversationIsStillLabelled(t *testing.T) {
	hits := []*lobslawv1.SessionSearchHitProto{{
		Session: &lobslawv1.SessionRecord{Id: "telegram:c1"},
		Matches: 1,
	}}
	var buf bytes.Buffer
	if err := renderSessionHits(&buf, hits, "x", "prod:9090", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "(untitled)") {
		t.Errorf("an untitled conversation has no label:\n%s", buf.String())
	}
}

// "no matches" must say where it looked, for the same reason an empty
// list does.
func TestNoMatchesStillNamesItsSource(t *testing.T) {
	var buf bytes.Buffer
	if err := renderSessionHits(&buf, nil, "pelican", "prod:9090", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "prod:9090") {
		t.Errorf("an empty search result does not say where it looked:\n%s", out)
	}
	if !strings.Contains(out, "no matches") {
		t.Errorf("the empty result is not stated:\n%s", out)
	}
}
