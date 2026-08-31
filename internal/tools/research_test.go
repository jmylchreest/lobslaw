package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

func researchTool(t *testing.T) (compute.BuiltinFunc, *fakeApplier) {
	t.Helper()
	applier := &fakeApplier{}
	b := NewBuiltins()
	if err := RegisterResearchBuiltins(b, ResearchConfig{Raft: applier}); err != nil {
		t.Fatalf("register: %v", err)
	}
	fn, ok := b.Get("research_start")
	if !ok {
		t.Fatal("research_start is not registered")
	}
	return fn, applier
}

// research_start schedules work that costs tokens and runs later, so
// the question has to survive into the commitment intact — a commitment
// whose question was lost runs and produces nothing anyone asked for.
func TestResearchStartRecordsTheQuestion(t *testing.T) {
	t.Parallel()

	fn, applier := researchTool(t)
	if _, code, err := fn(context.Background(), map[string]string{
		"question": "what changed in the Go 1.26 scheduler",
	}); err != nil || code != 0 {
		t.Fatalf("research_start: code=%d err=%v", code, err)
	}
	if len(applier.entries) == 0 {
		t.Fatal("nothing was applied; the research was never scheduled")
	}
	c := applier.entries[0].GetCommitment()
	if c == nil {
		t.Fatal("the applied entry is not a commitment")
	}
	if !strings.Contains(c.String(), "Go 1.26 scheduler") {
		t.Errorf("the question did not reach the commitment: %s", c.String())
	}
}

// An empty question is refused rather than scheduled. Exit 2 is the
// argument-error code the model can act on; 1 would read as a fault it
// should retry.
func TestResearchStartRefusesAnEmptyQuestion(t *testing.T) {
	t.Parallel()

	fn, applier := researchTool(t)
	for _, q := range []string{"", "   "} {
		_, code, err := fn(context.Background(), map[string]string{"question": q})
		if err == nil {
			t.Errorf("question %q was accepted", q)
		}
		if code != 2 {
			t.Errorf("question %q gave exit %d, want 2 (an argument error)", q, code)
		}
	}
	if len(applier.entries) != 0 {
		t.Error("a refused question still scheduled work")
	}
}

// Depth bounds the work the commitment will do, so an out-of-range
// value has to fail rather than be clamped silently into something the
// caller did not ask for.
func TestResearchStartValidatesDepth(t *testing.T) {
	t.Parallel()

	fn, _ := researchTool(t)
	for _, bad := range []string{"0", "11", "-1", "deep", "3.5"} {
		if _, code, err := fn(context.Background(), map[string]string{
			"question": "q", "depth": bad,
		}); err == nil || code != 2 {
			t.Errorf("depth %q was accepted (code=%d err=%v)", bad, code, err)
		}
	}
	for _, ok := range []string{"1", "5", "10"} {
		if _, code, err := fn(context.Background(), map[string]string{
			"question": "q", "depth": ok,
		}); err != nil || code != 0 {
			t.Errorf("depth %q was refused (code=%d err=%v)", ok, code, err)
		}
	}
}

// Registration needs somewhere to write. A tool that registers without
// one fails at the first call, on a turn the user is waiting for.
func TestResearchNeedsRaft(t *testing.T) {
	t.Parallel()

	if err := RegisterResearchBuiltins(NewBuiltins(), ResearchConfig{}); err == nil {
		t.Error("research_start registered with no raft behind it")
	}
}
