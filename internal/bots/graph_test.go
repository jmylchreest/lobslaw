package bots

import (
	"errors"
	"strings"
	"testing"
)

// The cycle rule is checked on WRITE, which is the whole reason
// may_message is a declaration rather than a free-for-all: a cycle
// caught when the edge is proposed is one message to one person about
// a decision they just made, and the same cycle caught at call time is
// a depth counter, a mid-conversation refusal, and a bill for however
// many turns ran before it tripped.

func TestAcyclicGraphIsAccepted(t *testing.T) {
	t.Parallel()
	g := Graph{
		"coordinator": {"engineering", "marketing"},
		"marketing":   {"engineering"},
		"engineering": nil,
	}
	if err := g.Validate(); err != nil {
		t.Errorf("a diamond is not a cycle: %v", err)
	}
}

func TestDirectCycleIsRefused(t *testing.T) {
	t.Parallel()
	g := Graph{
		"marketing":   {"engineering"},
		"engineering": {"marketing"},
	}
	err := g.Validate()
	if err == nil {
		t.Fatal("a two-bot loop was accepted")
	}
	var cycle *ErrCycle
	if !errors.As(err, &cycle) {
		t.Fatalf("err = %T, want *ErrCycle", err)
	}
	// Naming the path is the point: a user told only "invalid" has to
	// guess which of several edges was the problem.
	for _, want := range []string{"marketing", "engineering"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

func TestIndirectCycleIsRefused(t *testing.T) {
	t.Parallel()
	g := Graph{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
	}
	if err := g.Validate(); err == nil {
		t.Fatal("a three-hop loop was accepted")
	}
}

func TestSelfEdgeIsACycle(t *testing.T) {
	t.Parallel()
	if err := (Graph{"narcissus": {"narcissus"}}).Validate(); err == nil {
		t.Error("a bot pointing at itself was accepted")
	}
}

// An edge to a bot that does not exist yet is not a cycle. Refusing it
// here would make the order two bots are created in matter, and the
// registry checks existence separately.
func TestEdgeToAnUnknownBotIsNotACycle(t *testing.T) {
	t.Parallel()
	if err := (Graph{"coordinator": {"not-created-yet"}}).Validate(); err != nil {
		t.Errorf("an edge to an unknown bot was treated as a cycle: %v", err)
	}
}

// WithEdges is what a create or update proposes: the existing graph
// plus the one bot's new list, validated before anything is written.
func TestWithEdgesValidatesTheProposalNotTheStoredGraph(t *testing.T) {
	t.Parallel()
	stored := Graph{
		"coordinator": {"engineering"},
		"engineering": nil,
	}
	if err := stored.WithEdges("engineering", []string{"coordinator"}).Validate(); err == nil {
		t.Error("an update closing a loop was accepted")
	}
	if err := stored.Validate(); err != nil {
		t.Errorf("WithEdges mutated the stored graph: %v", err)
	}
}

// A validation error that names a different path each run is one
// nobody can act on or write a test for, and map iteration order in Go
// is deliberately random.
func TestCyclePathIsDeterministic(t *testing.T) {
	t.Parallel()
	g := Graph{
		"zulu":    {"alpha"},
		"alpha":   {"bravo"},
		"bravo":   {"zulu"},
		"charlie": {"alpha"},
	}
	first := g.Validate().Error()
	for range 20 {
		if got := g.Validate().Error(); got != first {
			t.Fatalf("path varies between runs:\n  %s\n  %s", first, got)
		}
	}
}

func TestEmptyGraphIsValid(t *testing.T) {
	t.Parallel()
	if err := (Graph{}).Validate(); err != nil {
		t.Errorf("an empty graph was rejected: %v", err)
	}
}
