package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/trace"
)

// `lobslaw trace` read a directory on the local filesystem, so on an
// operator's laptop it either found nothing or, worse, found a stale
// copy and reported it as the cluster's.

// --- which way the command goes ----------------------------------------

// THE WIRING. Without --offline the command must ask a NODE.
func TestEveryTraceFormIsLiveByDefault(t *testing.T) {
	for sub, form := range traceForms {
		if !routesTo(t, traceRoute(sub, false), form.live) {
			t.Errorf("%q without --offline does not reach its live form", sub)
		}
		if !routesTo(t, traceRoute(sub, true), form.offline) {
			t.Errorf("%q with --offline does not reach its offline form", sub)
		}
	}
}

func TestTheTraceFormsAreNotTheSame(t *testing.T) {
	for sub, form := range traceForms {
		if routesTo(t, form.live, form.offline) {
			t.Errorf("%q has the same function for live and offline", sub)
		}
	}
}

func TestTheTraceUsageMentionsBothPaths(t *testing.T) {
	for _, want := range []string{"--offline", "--context", "PER-NODE", "list"} {
		if !strings.Contains(traceUsage, want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
	// The no-content guarantee is the reason anybody trusts this
	// command with a production cluster.
	if !strings.Contains(traceUsage, "No span carries message text") {
		t.Error("usage no longer states that spans carry no content")
	}
}

// Without --offline, `trace list` must try to reach a node rather than
// resolve a local directory.
func TestTraceListGoesLiveByDefault(t *testing.T) {
	noAmbientCluster(t)
	t.Setenv("LOBSLAW_TRACE_DIR", "")

	err := traceListLive(nil)
	if err == nil {
		t.Fatal("list with no connection details succeeded")
	}
	if strings.Contains(err.Error(), "--dir") {
		t.Errorf("the live path asked for a local directory: %v", err)
	}
}

// A turn id is required, and a missing one must be an error rather
// than a panic.
func TestTraceShowNeedsATurnId(t *testing.T) {
	noAmbientCluster(t)
	if err := traceShowLive(nil); err == nil {
		t.Error("the live form accepted no turn id")
	}
	if err := traceShow(nil); err == nil {
		t.Error("the offline form accepted no turn id")
	}
}

// --- naming the node ---------------------------------------------------

// THE POINT OF THE BOX. Traces are per-node, so an answer that does not
// say whose it is invites exactly the wrong conclusion.
func TestTheSourceNamesTheNodeAndAddress(t *testing.T) {
	got := traceSource("node-a", "prod.example:9090")
	for _, want := range []string{"node-a", "prod.example:9090"} {
		if !strings.Contains(got, want) {
			t.Errorf("source %q is missing %q", got, want)
		}
	}
}

// A node that reported no id is still reachable at an address, and the
// address is what the operator typed. Printing nothing would be worse
// than printing half.
func TestASourceWithNoNodeIdStillNamesTheAddress(t *testing.T) {
	got := traceSource("", "prod.example:9090")
	if !strings.Contains(got, "prod.example:9090") {
		t.Errorf("source = %q", got)
	}
}

// --- the render --------------------------------------------------------

func spansForTurn() []trace.Span {
	return []trace.Span{
		{
			TurnID: "t1", SpanID: "s1", Kind: trace.KindLLMCall, Name: "opus",
			Provider: "anthropic", StartedAt: time.Now(), Duration: time.Second,
			Outcome: trace.OutcomeOK, CostUSD: 0.02,
			Usage: trace.Usage{PromptTokens: 1000, CompletionTokens: 200},
		},
		{
			TurnID: "t1", SpanID: "s2", Kind: trace.KindContextCarry,
			Name: "grep", StartedAt: time.Now(), Outcome: trace.OutcomeOK,
			CostUSD: 0.01, Usage: trace.Usage{PromptTokens: 4000}, Quantity: 5, Unit: "prompts",
		},
	}
}

func TestTheTurnRenderNamesItsSource(t *testing.T) {
	var buf bytes.Buffer
	renderTurn(&buf, "node node-a (prod:9090)", "t1", spansForTurn())
	out := buf.String()
	if !strings.Contains(out, "node-a") {
		t.Errorf("the turn does not say which node recorded it:\n%s", out)
	}
	if !strings.Contains(out, "turn t1") {
		t.Errorf("the turn id is missing:\n%s", out)
	}
}

// A context-carry span ATTRIBUTES cost the LLM spans already counted;
// summing both would roughly double the turn and make the one command
// whose job is "why did this cost what it did" answer it wrongly.
func TestTheCarryCostIsNotAddedToTheTotal(t *testing.T) {
	var buf bytes.Buffer
	renderTurn(&buf, "node-a", "t1", spansForTurn())
	out := buf.String()

	if !strings.Contains(out, "$0.0200") {
		t.Errorf("the total is not the LLM cost alone:\n%s", out)
	}
	if strings.Contains(out, "$0.0300") {
		t.Errorf("the carry cost was added to the total:\n%s", out)
	}
	if !strings.Contains(out, "re-sent tool output") {
		t.Errorf("the attributed share is not reported:\n%s", out)
	}
}

// The renderer must be the same one the offline path uses, or a remote
// turn shows a different cost from the same turn read locally.
func TestARemoteTurnRendersLikeALocalOne(t *testing.T) {
	spans := spansForTurn()

	var local, remote bytes.Buffer
	renderTurn(&local, "X", "t1", spans)

	// Through the wire and back.
	roundTripped := make([]trace.Span, 0, len(spans))
	for _, s := range spans {
		roundTripped = append(roundTripped, trace.SpanFromProto(trace.SpanToProto(s)))
	}
	renderTurn(&remote, "X", "t1", roundTripped)

	if local.String() != remote.String() {
		t.Errorf("a round-tripped turn renders differently:\nlocal:\n%s\nremote:\n%s",
			local.String(), remote.String())
	}
}
