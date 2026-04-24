package compute

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// agentEnv stacks the mock provider + an Executor (via newTestEnv)
// + a fresh Agent so tests can poke at the full pipeline.
type agentEnv struct {
	*testEnv
	mock  *MockProvider
	agent *Agent
}

// newAgentEnv builds a ready-to-run Agent backed by a scripted
// mock LLM. Tests pass the scripted responses as arguments; the
// Executor defaults to no-sandbox (fleet-wide) for simplicity.
func newAgentEnv(t *testing.T, responses ...MockResponse) *agentEnv {
	t.Helper()
	base := newTestEnv(t)
	mock := NewMockProvider(responses...)
	agent, err := NewAgent(AgentConfig{
		Provider: mock,
		Executor: base.executor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &agentEnv{testEnv: base, mock: mock, agent: agent}
}

// mkBudget is a tiny helper so tests aren't littered with
// zero-value budget constructions.
func mkBudget(t *testing.T, caps BudgetCaps) *TurnBudget {
	t.Helper()
	b, err := NewTurnBudget(caps)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// echoBinary finds a system echo binary (/bin/echo or /usr/bin/echo).
// Previous versions of these tests wrote a shell script to tempdir
// then immediately execed it — race-prone on some filesystems (the
// fs cache may not expose the new executable mode bits in time).
// Using a pre-existing system binary removes that variable entirely.
// Skips the test on platforms without either path (Windows, minimal
// containers) rather than failing on irrelevant environments.
func echoBinary(t *testing.T) string {
	t.Helper()
	for _, p := range []string{"/bin/echo", "/usr/bin/echo"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return p
		}
	}
	t.Skip("no system /bin/echo or /usr/bin/echo available (Windows or minimal container?)")
	return ""
}

// registerEchoTool registers an "echo" tool backed by the system
// echo binary. Used across several tests that need a deterministic,
// fast subprocess that prints its first argument.
func registerEchoTool(t *testing.T, env *agentEnv) {
	t.Helper()
	path := echoBinary(t)
	if err := env.reg.Register(&types.ToolDef{
		Name:         "echo",
		Path:         path,
		ArgvTemplate: []string{"{msg}"},
		RiskTier:     types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}
}

// registerNoopTool registers a zero-output tool backed by /bin/true
// (or equivalent). Used by tests that want a tool that succeeds
// without caring about output.
func registerNoopTool(t *testing.T, env *agentEnv, name string) {
	t.Helper()
	var path string
	for _, p := range []string{"/bin/true", "/usr/bin/true"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			path = p
			break
		}
	}
	if path == "" {
		t.Skip("no /bin/true equivalent available")
	}
	if err := env.reg.Register(&types.ToolDef{
		Name:     name,
		Path:     path,
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNewAgentRequiresProvider(t *testing.T) {
	t.Parallel()
	_, err := NewAgent(AgentConfig{})
	if !errors.Is(err, ErrNoLLMProvider) {
		t.Errorf("want ErrNoLLMProvider; got %v", err)
	}
}

// TestRunToolCallLoopTextOnlyReply — the simplest path: LLM
// returns text-only, agent loop exits immediately with the reply.
func TestRunToolCallLoopTextOnlyReply(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t, MockResponse{Content: "hello from the mock"})

	resp, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message:      "hi",
		Claims:       &types.Claims{UserID: "alice"},
		TurnID:       "t1",
		SystemPrompt: "you are a test bot",
		Budget:       mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reply != "hello from the mock" {
		t.Errorf("reply: %q", resp.Reply)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("no tool calls expected; got %v", resp.ToolCalls)
	}
	if resp.NeedsConfirmation {
		t.Error("no confirmation expected")
	}
	if env.mock.CallCount() != 1 {
		t.Errorf("want 1 LLM call; got %d", env.mock.CallCount())
	}
}

// TestRunToolCallLoopSystemPromptIsFirstMessage verifies the
// prompt-seeding logic places the system prompt at the top of the
// assembled messages.
func TestRunToolCallLoopSystemPromptIsFirstMessage(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t, MockResponse{Content: "ok"})

	_, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message:      "hi",
		SystemPrompt: "SYSTEM-PROMPT-CONTENT",
		Claims:       &types.Claims{UserID: "alice"},
		Budget:       mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := env.mock.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call; got %d", len(calls))
	}
	if len(calls[0].Messages) == 0 {
		t.Fatal("no messages captured")
	}
	first := calls[0].Messages[0]
	if first.Role != "system" || first.Content != "SYSTEM-PROMPT-CONTENT" {
		t.Errorf("first message not the system prompt: %+v", first)
	}
}

// TestRunToolCallLoopToolCallRoundTrip — scripted scenario: first
// response asks to call 'echo', second response returns text after
// seeing the tool output. Validates the full loop: LLM → Executor →
// LLM → done.
func TestRunToolCallLoopToolCallRoundTrip(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t,
		MockResponse{ToolCalls: []ToolCall{
			{ID: "call-1", Name: "echo", Arguments: `{"msg":"hi"}`},
		}},
		MockResponse{Content: "the tool said hi"},
	)

	registerEchoTool(t, env)

	resp, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message:      "say hi",
		Claims:       &types.Claims{UserID: "alice", Scope: "*"},
		TurnID:       "t1",
		SystemPrompt: "you are test",
		Budget:       mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Reply != "the tool said hi" {
		t.Errorf("final reply: %q", resp.Reply)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call; got %d", len(resp.ToolCalls))
	}
	inv := resp.ToolCalls[0]
	if inv.ToolName != "echo" {
		t.Errorf("tool call name: %q", inv.ToolName)
	}
	if !strings.Contains(inv.Output, "hi") {
		// Rich diagnostic: if this ever flakes again we want to
		// know exactly why — exit code, any invocation error, and
		// the actual captured output.
		t.Errorf("tool output should contain 'hi': output=%q exit=%d err=%q",
			inv.Output, inv.ExitCode, inv.Error)
	}
	if env.mock.CallCount() != 2 {
		t.Errorf("expected 2 LLM calls; got %d", env.mock.CallCount())
	}
}

// TestRunToolCallLoopToolResultFedBackAsToolRole confirms the
// tool's output becomes a role="tool" message in the NEXT LLM
// round-trip, with the correct ToolCallID correlation.
func TestRunToolCallLoopToolResultFedBackAsToolRole(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t,
		MockResponse{ToolCalls: []ToolCall{
			{ID: "abc", Name: "echo", Arguments: `{"msg":"ok"}`},
		}},
		MockResponse{Content: "done"},
	)

	registerEchoTool(t, env)

	_, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message: "go",
		Claims:  &types.Claims{UserID: "alice", Scope: "*"},
		Budget:  mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := env.mock.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 LLM calls; got %d", len(calls))
	}
	// Second call should have: user, assistant (with tool-call), tool (output).
	secondCall := calls[1]
	var toolMsg *Message
	for i := range secondCall.Messages {
		if secondCall.Messages[i].Role == "tool" {
			toolMsg = &secondCall.Messages[i]
			break
		}
	}
	if toolMsg == nil {
		t.Fatalf("expected a tool-role message in round 2; got %+v", secondCall.Messages)
	}
	if toolMsg.ToolCallID != "abc" {
		t.Errorf("ToolCallID: %q", toolMsg.ToolCallID)
	}
	if !strings.Contains(toolMsg.Content, "<untrusted source=") {
		t.Errorf("tool output should be wrapped in untrusted delimiters; got %q", toolMsg.Content)
	}
}

// TestRunToolCallLoopBudgetExceededOnToolCallSurfacesConfirmation —
// budget exceeded DURING the loop returns NeedsConfirmation with
// the ExceededOn dimension in the reason, doesn't error.
func TestRunToolCallLoopBudgetZeroCapsMeanUnlimited(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t,
		MockResponse{ToolCalls: []ToolCall{
			{ID: "x", Name: "echo", Arguments: `{}`},
		}},
		// Second response after the tool runs → text-only, loop ends.
		MockResponse{Content: "done"},
	)

	registerNoopTool(t, env, "echo")

	resp, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message: "do something",
		Claims:  &types.Claims{UserID: "alice", Scope: "*"},
		Budget:  mkBudget(t, BudgetCaps{MaxToolCalls: 0}), // 0 = unlimited
	})
	if err != nil {
		t.Fatal(err)
	}
	// With MaxToolCalls=0 (unlimited), no confirmation expected —
	// validates the zero-means-unlimited semantic plumbs through.
	if resp.NeedsConfirmation {
		t.Error("zero caps should not trigger confirmation")
	}
	if resp.Reply != "done" {
		t.Errorf("final reply: %q", resp.Reply)
	}
}

func TestRunToolCallLoopBudgetCapOfOneConfirmsOnSecondCall(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t,
		MockResponse{ToolCalls: []ToolCall{
			{ID: "1", Name: "echo", Arguments: `{}`},
		}},
		MockResponse{ToolCalls: []ToolCall{
			{ID: "2", Name: "echo", Arguments: `{}`},
		}},
	)
	registerNoopTool(t, env, "echo")

	resp, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice", Scope: "*"},
		Budget: mkBudget(t, BudgetCaps{MaxToolCalls: 1}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.NeedsConfirmation {
		t.Fatal("second tool call should trip MaxToolCalls=1")
	}
	if !strings.Contains(resp.ConfirmationReason, "tool_calls") {
		t.Errorf("confirmation reason: %q", resp.ConfirmationReason)
	}
}

func TestRunToolCallLoopMaxToolLoopsForcesSummary(t *testing.T) {
	t.Parallel()
	// Script emits tool calls until MaxToolLoops is reached, then
	// the forced-summary call (with tools stripped) returns text.
	// Confirms the loop exhausts into a user-facing reply rather
	// than returning a silent error.
	env := newAgentEnv(t)
	env.mock = NewMockProviderFunc(func(req ChatRequest, idx int) (MockResponse, error) {
		// When the forced-summary pass runs, tools are stripped from
		// the request — detect that and return a plain-text reply.
		if len(req.Tools) == 0 {
			return MockResponse{Content: "I tried several tools and couldn't finish — here's what I found..."}, nil
		}
		return MockResponse{ToolCalls: []ToolCall{
			{ID: fmt.Sprintf("c-%d", idx), Name: "noop", Arguments: `{}`},
		}}, nil
	})
	env.agent, _ = NewAgent(AgentConfig{
		Provider:     env.mock,
		Executor:     env.executor,
		MaxToolLoops: 3,
	})

	registerNoopTool(t, env, "noop")

	resp, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice", Scope: "*"},
		Budget: mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatalf("expected forced-summary success; got err %v", err)
	}
	if resp.Reply == "" {
		t.Error("forced summary should produce a user-visible reply")
	}
	if !strings.Contains(resp.Reply, "tried several tools") {
		t.Errorf("expected summary content; got %q", resp.Reply)
	}
}

func TestRunToolCallLoopParseToolArgsHandlesNonStringValues(t *testing.T) {
	t.Parallel()
	// Numbers and bools should stringify cleanly.
	got, err := parseToolArgs(`{"n": 42, "enabled": true, "name": "bob"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got["n"] != "42" || got["enabled"] != "true" || got["name"] != "bob" {
		t.Errorf("got %+v", got)
	}
}

func TestRunToolCallLoopParseToolArgsEmptyAndMalformed(t *testing.T) {
	t.Parallel()
	// Empty → empty map.
	if m, err := parseToolArgs(""); err != nil || len(m) != 0 {
		t.Errorf("empty args: %v %v", m, err)
	}
	if m, err := parseToolArgs("{}"); err != nil || len(m) != 0 {
		t.Errorf("empty object: %v %v", m, err)
	}
	// Malformed → error.
	if _, err := parseToolArgs("not json"); err == nil {
		t.Error("malformed JSON should error")
	}
}

// TestRunToolCallLoopToolErrorBecomesToolMessage — when Executor
// returns an error (policy denied, tool not found), the agent
// feeds the error string to the LLM as a tool-role message rather
// than returning the error. Lets the model recover ("try a
// different tool") instead of killing the turn.
func TestRunToolCallLoopToolErrorBecomesToolMessage(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t,
		MockResponse{ToolCalls: []ToolCall{
			{ID: "bad", Name: "nonexistent-tool", Arguments: `{}`},
		}},
		MockResponse{Content: "I saw the tool wasn't available"},
	)
	resp, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice", Scope: "*"},
		Budget: mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call; got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Error == "" {
		t.Error("tool-not-found should surface as inv.Error")
	}
	if resp.Reply != "I saw the tool wasn't available" {
		t.Errorf("final reply: %q", resp.Reply)
	}
}

func TestRunToolCallLoopConversationHistoryPreserved(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t, MockResponse{Content: "reply"})
	history := []Message{
		{Role: "user", Content: "previous turn"},
		{Role: "assistant", Content: "previous reply"},
	}
	_, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message:             "now",
		ConversationHistory: history,
		Claims:              &types.Claims{UserID: "alice"},
		Budget:              mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := env.mock.Calls()
	// First call's messages should include the history + "now".
	msgs := calls[0].Messages
	var seenPrev, seenNow bool
	for _, m := range msgs {
		if m.Content == "previous turn" {
			seenPrev = true
		}
		if m.Content == "now" {
			seenNow = true
		}
	}
	if !seenPrev || !seenNow {
		t.Errorf("history not passed through: messages=%+v", msgs)
	}
}

// TestRunToolCallLoopResponseMessagesReturnedForPersistence confirms
// the full conversation-after-turn is returned — callers persist
// this for the next turn.
func TestRunToolCallLoopResponseMessagesReturnedForPersistence(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t, MockResponse{Content: "done"})
	resp, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message: "hi",
		Claims:  &types.Claims{UserID: "alice"},
		Budget:  mkBudget(t, BudgetCaps{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Messages) < 2 {
		t.Errorf("expected at least [user, assistant]; got %+v", resp.Messages)
	}
	// Last message should be the assistant reply.
	last := resp.Messages[len(resp.Messages)-1]
	if last.Role != "assistant" || last.Content != "done" {
		t.Errorf("last message should be assistant reply: %+v", last)
	}
}

func TestRunToolCallLoopRequiresBudget(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t, MockResponse{Content: "x"})
	_, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Message: "hi",
		Claims:  &types.Claims{UserID: "alice"},
	})
	if err == nil {
		t.Error("nil budget should be rejected")
	}
}

// TestRunToolCallLoopLLMErrorPropagates — a provider returning an
// error (network down, rate-limited) kills the turn with a wrapped
// error. Distinct from tool errors, which the agent surfaces to
// the LLM as a retryable tool-role message.
func TestRunToolCallLoopLLMErrorPropagates(t *testing.T) {
	t.Parallel()
	env := newAgentEnv(t,
		MockResponse{Err: errors.New("provider exploded")},
	)
	_, err := env.agent.RunToolCallLoop(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice"},
		Budget: mkBudget(t, BudgetCaps{}),
	})
	if err == nil || !strings.Contains(err.Error(), "exploded") {
		t.Errorf("provider error should propagate; got %v", err)
	}
}
