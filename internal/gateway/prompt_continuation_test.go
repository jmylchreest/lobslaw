package gateway

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A confirmation is a turn stopped mid-flight. It used to be stopped
// inside one process's memory, so the two failures below were both
// reachable: approve on a node that did not ask and nothing happens;
// approve after a restart and the user is told to send it again.

// twoNodes returns two registries over one raft cluster — the
// stand-in for a keyboard sent by one node and tapped on another. The
// second also stands in for the same node after a restart: it shares
// no in-process state with the first.
func twoNodes(t *testing.T, caps compute.BudgetCaps) (asker, answerer *RaftPrompts) {
	t.Helper()
	dataDir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dataDir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	_, inmem := raft.NewInmemTransport("cont-node")
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "cont-node", LocalAddr: "cont-node",
		DataDir: dataDir, Bootstrap: true, Transport: inmem,
	}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) *RaftPrompts {
		ps, err := memory.NewPromptStore(memory.PromptStoreConfig{Raft: node, Store: store})
		if err != nil {
			t.Fatal(err)
		}
		return NewRaftPrompts(ps, id, caps)
	}
	return mk("node-a"), mk("node-b")
}

func pausedTurn() *Continuation {
	budget, _ := compute.NewTurnBudget(compute.BudgetCaps{MaxToolCalls: 10, MaxSpendUSD: 1})
	budget.RecordToolCall()
	budget.RecordToolCall()
	budget.RecordToolCall()
	budget.RecordCostUSD(compute.CostRecord{CostUSD: 0.25})
	return &Continuation{
		Request: compute.ProcessMessageRequest{
			Message:             "tidy my notes",
			Claims:              &types.Claims{UserID: "tg-@alice", Roles: []string{"ops"}, Scope: "private"},
			UserTimezone:        "Europe/London",
			Model:               "claude-haiku-4-5",
			SystemPrompt:        "you are lobslaw",
			ConversationSummary: "earlier: talked about notes",
			RecalledContext:     "<recall>notes live in workspace</recall>",
			Budget:              budget,
		},
		Messages: []compute.Message{
			{Role: "user", Content: "tidy my notes"},
			{Role: "assistant", ToolCalls: []compute.ToolCall{
				{ID: "c1", Name: "write_file", Arguments: `{"path":"notes/plan.md"}`},
			}},
			{Role: "tool", ToolCallID: "c1", Content: "needs confirmation"},
		},
	}
}

// The whole point: a turn paused by one process is resumable by
// another. Under the old map this returned "I've lost track of that
// turn — send it again."
func TestPausedTurnSurvivesTheProcessThatPausedIt(t *testing.T) {
	t.Parallel()
	asker, answerer := twoNodes(t, compute.BudgetCaps{MaxToolCalls: 10, MaxSpendUSD: 1})

	created, err := asker.Create(NewPrompt{
		TurnID:       "turn-7",
		SessionID:    "sess-3",
		Reason:       "write to your notes?",
		Channel:      "telegram",
		ChannelID:    "-100",
		TTL:          time.Minute,
		Action:       "tool:exec",
		Resource:     "write_file",
		Continuation: pausedTurn(),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := answerer.Get(created.ID)
	if err != nil {
		t.Fatalf("the other node cannot see the prompt: %v", err)
	}
	if got.TurnID != "turn-7" || got.SessionID != "sess-3" || got.ChannelID != "-100" {
		t.Errorf("routing lost: turn=%q session=%q channel_id=%q",
			got.TurnID, got.SessionID, got.ChannelID)
	}
	if got.Action != "tool:exec" || got.Resource != "write_file" {
		t.Errorf("operation lost: %q / %q — a scoped answer has nothing to grant against",
			got.Action, got.Resource)
	}

	c := got.Continuation
	if c == nil {
		t.Fatal("continuation lost; the other node has nothing to resume")
	}
	if len(c.Messages) != 3 {
		t.Fatalf("transcript is %d messages, want 3: %+v", len(c.Messages), c.Messages)
	}
	if c.Messages[1].ToolCalls[0].Arguments != `{"path":"notes/plan.md"}` {
		t.Errorf("tool call arguments lost: %+v", c.Messages[1].ToolCalls)
	}
	if c.Messages[2].ToolCallID != "c1" {
		t.Errorf("tool result lost its correlation id: %+v", c.Messages[2])
	}

	r := c.Request
	if r.Message != "tidy my notes" {
		t.Errorf("user message lost: %q", r.Message)
	}
	if r.Claims == nil || r.Claims.UserID != "tg-@alice" || r.Claims.Scope != "private" {
		t.Errorf("claims lost: %+v — the resumed turn would evaluate policy as somebody else", r.Claims)
	}
	if len(r.Claims.Roles) != 1 || r.Claims.Roles[0] != "ops" {
		t.Errorf("roles lost: %+v", r.Claims.Roles)
	}
	if r.SystemPrompt != "you are lobslaw" {
		t.Errorf("system prompt lost: %q — the turn would finish as a different assistant", r.SystemPrompt)
	}
	if r.UserTimezone != "Europe/London" || r.Model != "claude-haiku-4-5" {
		t.Errorf("turn settings lost: tz=%q model=%q", r.UserTimezone, r.Model)
	}
	if r.ConversationSummary == "" || r.RecalledContext == "" {
		t.Errorf("context lost: summary=%q recall=%q", r.ConversationSummary, r.RecalledContext)
	}
	if r.Tools != nil {
		t.Error("tools were carried across; they are node state and must be rebuilt from the local registry")
	}
}

// A confirmation must not be a way to get a fresh allowance. Pause a
// turn near its cap, resume it, and the spend has to still be there.
func TestResumingDoesNotRefillTheBudget(t *testing.T) {
	t.Parallel()
	caps := compute.BudgetCaps{MaxToolCalls: 10, MaxSpendUSD: 1}
	asker, answerer := twoNodes(t, caps)

	created, err := asker.Create(NewPrompt{
		TurnID: "turn-8", Reason: "r", Channel: "telegram",
		ChannelID: "-100", TTL: time.Minute,
		Continuation: pausedTurn(),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := answerer.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Continuation == nil {
		t.Fatal("continuation lost; there is no budget to check")
	}
	state := got.Continuation.Request.Budget.State()
	if state.ToolCalls != 3 {
		t.Errorf("tool calls = %d, want 3 — resuming reset the counter", state.ToolCalls)
	}
	if state.SpendUSD != 0.25 {
		t.Errorf("spend = %v, want 0.25 — resuming reset the spend", state.SpendUSD)
	}
}

// The caps come from the resuming node, not the record. An operator
// who lowers a limit should not be overridden by a turn that started
// before the change.
func TestResumingUsesTheCurrentCaps(t *testing.T) {
	t.Parallel()
	// The turn was paused under a 10-call cap; this node now allows 4.
	asker, answerer := twoNodes(t, compute.BudgetCaps{MaxToolCalls: 4})

	created, err := asker.Create(NewPrompt{
		TurnID: "turn-9", Reason: "r", Channel: "telegram",
		ChannelID: "-100", TTL: time.Minute,
		Continuation: pausedTurn(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := answerer.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Continuation == nil {
		t.Fatal("continuation lost; there is no budget to check")
	}

	// Three calls already spent against a cap of four: one more is
	// within, the next is not.
	budget := got.Continuation.Request.Budget
	if d := budget.RecordToolCall(); !d.Within {
		t.Fatalf("the 4th call was refused under a cap of 4: %+v", d)
	}
	if d := budget.RecordToolCall(); d.Within {
		t.Errorf("the 5th call was permitted under a cap of 4 — the paused turn's old cap won: %+v", d)
	}
}

// Restore must never lower a counter. Otherwise a resume carrying a
// smaller figure would hand back the difference.
func TestBudgetRestoreOnlyMovesForward(t *testing.T) {
	t.Parallel()
	budget, err := compute.NewTurnBudget(compute.BudgetCaps{MaxToolCalls: 10})
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		budget.RecordToolCall()
	}
	budget.RecordCostUSD(compute.CostRecord{CostUSD: 0.5})

	budget.Restore(compute.BudgetState{ToolCalls: 1, SpendUSD: 0.1})

	state := budget.State()
	if state.ToolCalls != 5 {
		t.Errorf("tool calls = %d, want 5 — Restore handed back spent budget", state.ToolCalls)
	}
	if state.SpendUSD != 0.5 {
		t.Errorf("spend = %v, want 0.5 — Restore handed back spent budget", state.SpendUSD)
	}
}

// A prompt with no continuation is legitimate — REST resumes in the
// request that raised it — and must not be reported as a broken one.
func TestPromptWithoutAContinuationIsFine(t *testing.T) {
	t.Parallel()
	asker, answerer := twoNodes(t, compute.BudgetCaps{})

	created, err := asker.Create(NewPrompt{
		TurnID: "turn-10", Reason: "r", Channel: "rest", TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := answerer.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Continuation != nil {
		t.Errorf("a continuation appeared from nowhere: %+v", got.Continuation)
	}
}

// A scoped answer has to survive the round trip, or "for this chat"
// and "always" become indistinguishable from "once" on any node that
// reads the record back.
func TestScopeRoundTrips(t *testing.T) {
	t.Parallel()
	asker, answerer := twoNodes(t, compute.BudgetCaps{})

	for _, want := range []PromptScope{PromptScopeOnce, PromptScopeSession, PromptScopeAlways} {
		created, err := asker.Create(NewPrompt{
			TurnID: "t", Reason: "r", Channel: "telegram", TTL: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := answerer.Resolve(created.ID, PromptApproved, want); err != nil {
			t.Fatal(err)
		}
		got, err := asker.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Scope != want {
			t.Errorf("scope = %v, want %v", got.Scope, want)
		}
	}
}

// "No, and never again" is not something any button offers, so a
// denial must not record a standing scope.
func TestDenialHasNoLastingScope(t *testing.T) {
	t.Parallel()
	asker, answerer := twoNodes(t, compute.BudgetCaps{})

	created, err := asker.Create(NewPrompt{
		TurnID: "t", Reason: "r", Channel: "telegram", TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := answerer.Resolve(created.ID, PromptDenied, PromptScopeAlways); err != nil {
		t.Fatal(err)
	}
	got, err := asker.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Scope != PromptScopeOnce {
		t.Errorf("a denial recorded scope %v; nothing offers a standing refusal", got.Scope)
	}
}

// An unrecognised scope on the wire must narrow, never widen.
func TestUnknownScopeParsesAsOnce(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "once", "ALWAYS", "forever", "sess", "always "} {
		if got := ParsePromptScope(in); in != "once" && got != PromptScopeOnce {
			t.Errorf("ParsePromptScope(%q) = %v, want once", in, got)
		}
	}
	if ParsePromptScope("session") != PromptScopeSession {
		t.Error("session did not parse")
	}
	if ParsePromptScope("always") != PromptScopeAlways {
		t.Error("always did not parse")
	}
}

func TestPreparedInputSurvivesDurableContinuation(t *testing.T) {
	asker, answerer := twoNodes(t, compute.BudgetCaps{MaxToolCalls: 10, MaxSpendUSD: 1})
	cont := pausedTurn()
	cont.Messages[2].PreparedToolCall = &compute.PreparedToolCall{
		CallID: "c1", ToolName: "write_file", TurnID: "turn-7", OriginalArguments: cont.Messages[1].ToolCalls[0].Arguments,
		Params:    map[string]string{"path": "notes/rewritten.md"},
		Approvals: []compute.PreparedApproval{{Action: "tool:exec", Resource: "write_file"}},
	}
	created, err := asker.Create(NewPrompt{TurnID: "turn-7", SessionID: "sess-3", Channel: "telegram", ChannelID: "-100", TTL: time.Minute, Continuation: cont})
	if err != nil {
		t.Fatal(err)
	}
	got, err := answerer.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := got.Continuation.Messages[2].PreparedToolCall
	if p == nil || p.CallID != "c1" || p.ToolName != "write_file" || p.TurnID != "turn-7" || p.Params["path"] != "notes/rewritten.md" || p.OriginalArguments != cont.Messages[1].ToolCalls[0].Arguments || len(p.Approvals) != 1 || p.Approvals[0].Action != "tool:exec" {
		t.Fatalf("prepared input lost: %+v", p)
	}
	p.Params["path"] = "changed"
	if cont.Messages[2].PreparedToolCall.Params["path"] != "notes/rewritten.md" {
		t.Fatal("continuation aliases input")
	}
}

func TestLegacyContinuationSeesEffectiveArguments(t *testing.T) {
	cont := pausedTurn()
	original := cont.Messages[1].ToolCalls[0].Arguments
	cont.Messages[2].PreparedToolCall = &compute.PreparedToolCall{CallID: "c1", ToolName: "write_file", OriginalArguments: original, Params: map[string]string{"path": "rewritten"}}
	wire := continuationToProto(cont)
	if wire.Messages[1].ToolCalls[0].Arguments != `{"path":"rewritten"}` {
		t.Fatalf("old reader sees %s", wire.Messages[1].ToolCalls[0].Arguments)
	}
	restored, err := continuationFromProto(wire, compute.BudgetCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Messages[1].ToolCalls[0].Arguments != original {
		t.Fatal("new reader lost original binding")
	}
	wire.Messages[2].PreparedToolCall = nil
	legacy, err := continuationFromProto(wire, compute.BudgetCaps{})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Messages[1].ToolCalls[0].Arguments != `{"path":"rewritten"}` {
		t.Fatal("legacy resume lost effective input")
	}
}
