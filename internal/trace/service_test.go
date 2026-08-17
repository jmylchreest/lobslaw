package trace

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `lobslaw trace` read a directory on the local filesystem, so on an
// operator's laptop it either found nothing or, worse, found a stale
// copy and reported it as the cluster's. Traces are per-node files by
// design — the fix is being able to ask a SPECIFIC node, and having
// every answer say which one.

func recordTurn(t *testing.T, dir string, spans ...Span) {
	t.Helper()
	sink, err := NewFileSink(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range spans {
		if werr := sink.Write(s); werr != nil {
			t.Fatal(werr)
		}
	}
	if cerr := sink.Close(); cerr != nil {
		t.Fatal(cerr)
	}
}

func demoSpan(turnID, name string) Span {
	return Span{
		TurnID: turnID, SpanID: name, Kind: KindLLMCall, Name: name,
		Provider: "anthropic", StartedAt: time.Now(), Duration: 250 * time.Millisecond,
		Outcome: OutcomeOK, CostUSD: 0.0125,
		Usage: Usage{PromptTokens: 1200, CompletionTokens: 300, CachedTokens: 900},
	}
}

// THE POINT OF THE BOX. Every answer names the node.
func TestTheListingNamesTheNode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	recordTurn(t, dir, demoSpan("turn-1", "opus"))
	svc := NewService("node-a", dir, true)

	res, err := svc.ListTurns(context.Background(), &lobslawv1.ListTurnsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodeId() != "node-a" {
		t.Errorf("node_id = %q; a per-node listing without it is unattributable", res.GetNodeId())
	}
	if len(res.GetTurnIds()) != 1 || res.GetTurnIds()[0] != "turn-1" {
		t.Errorf("turn_ids = %v", res.GetTurnIds())
	}
}

func TestReadingATurnNamesTheNode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	recordTurn(t, dir, demoSpan("turn-1", "opus"))
	svc := NewService("node-a", dir, true)

	res, err := svc.ReadTurn(context.Background(), &lobslawv1.ReadTurnRequest{TurnId: "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.GetNodeId() != "node-a" {
		t.Errorf("node_id = %q", res.GetNodeId())
	}
	if len(res.GetSpans()) != 1 {
		t.Fatalf("got %d spans", len(res.GetSpans()))
	}
}

// A node with tracing OFF and a node that has served no turns both
// return nothing, and they are different answers — only one is fixed
// by editing config.
func TestTracingOffIsDistinguishedFromNoTurns(t *testing.T) {
	t.Parallel()

	off := NewService("node-a", t.TempDir(), false)
	res, err := off.ListTurns(context.Background(), &lobslawv1.ListTurnsRequest{})
	if err != nil {
		t.Fatalf("a node with tracing off returned an error: %v", err)
	}
	if res.GetEnabled() {
		t.Error("a node with tracing off reported it as enabled")
	}

	quiet := NewService("node-a", t.TempDir(), true)
	res, err = quiet.ListTurns(context.Background(), &lobslawv1.ListTurnsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.GetEnabled() {
		t.Error("a node with tracing on reported it as disabled")
	}
	if len(res.GetTurnIds()) != 0 {
		t.Errorf("turn_ids = %v on an empty directory", res.GetTurnIds())
	}
}

// Tracing off is a configuration, not a failure. Erroring would make a
// deliberate setting look broken.
func TestReadingFromANodeWithTracingOffIsNotAnError(t *testing.T) {
	t.Parallel()
	svc := NewService("node-a", t.TempDir(), false)

	res, err := svc.ReadTurn(context.Background(), &lobslawv1.ReadTurnRequest{TurnId: "turn-1"})
	if err != nil {
		t.Fatalf("tracing off produced an error: %v", err)
	}
	if res.GetEnabled() || len(res.GetSpans()) != 0 {
		t.Errorf("res = %+v", res)
	}
	if res.GetNodeId() != "node-a" {
		t.Error("even the empty answer must say which node it is about")
	}
}

func TestReadTurnNeedsATurnId(t *testing.T) {
	t.Parallel()
	svc := NewService("node-a", t.TempDir(), true)
	_, err := svc.ReadTurn(context.Background(), &lobslawv1.ReadTurnRequest{})
	if err == nil {
		t.Fatal("an empty turn id was accepted")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
}

// --- the wire round trip -----------------------------------------------

// Every measured field must survive, or a remote turn shows a
// different cost from the same turn read locally — and the cost is the
// only reason anybody opens this command.
func TestASpanSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	want := Span{
		TurnID: "turn-1", SpanID: "s1", ParentID: "s0", Kind: KindLLMCall,
		Name: "opus", Provider: "anthropic",
		StartedAt: time.Now().UTC().Truncate(time.Microsecond),
		Duration:  1500 * time.Millisecond, Outcome: OutcomeOK,
		Usage:   Usage{PromptTokens: 1200, CompletionTokens: 300, CachedTokens: 900},
		CostUSD: 0.0125, ResultSize: 4096,
		Unit: "images", Quantity: 3, BilledTo: "plan",
		Error: "429 rate_limited", Attempt: 2,
	}

	got := SpanFromProto(SpanToProto(want))
	if got != want {
		t.Errorf("round trip changed the span:\n got %+v\nwant %+v", got, want)
	}
}

// A zero span must stay zero.
func TestAZeroSpanStaysZero(t *testing.T) {
	t.Parallel()
	got := SpanFromProto(SpanToProto(Span{}))
	if got != (Span{}) {
		t.Errorf("a zero span came back as %+v", got)
	}
}

// A span with no start time must not CLAIM one on the wire.
//
// The round trip survives either way — a zero time marshals and
// unmarshals to a zero time — so this asserts the wire field itself.
// An always-set timestamp reads as "this span started in year 1",
// which a consumer sorting by start time would believe.
func TestASpanWithNoStartTimeLeavesTheFieldUnset(t *testing.T) {
	t.Parallel()
	if got := SpanToProto(Span{}); got.GetStartedAt() != nil {
		t.Errorf("started_at = %v on a span that never started", got.GetStartedAt())
	}
	if got := SpanToProto(Span{StartedAt: time.Now()}); got.GetStartedAt() == nil {
		t.Error("started_at is unset on a span that did start")
	}
}
