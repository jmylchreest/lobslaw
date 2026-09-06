package compute

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A FAILED TOOL CALL USED TO REACH ONLY THE MODEL.
//
// When a tool fails, the loop puts the complaint on the invocation and
// hands it to the model, which reads it, adapts, and answers. That is
// the design and it works. What it also did was end there: no line
// reached the operator log, so `podman logs` showed a clean turn while
// the agent was being refused by every site it tried.
//
// The case that hid longest is the one WITHOUT an error. A tool that
// runs and fails returns a normal result carrying a non-zero exit and
// its complaint on stderr — err is nil, nothing unwinds, and every
// check for "did Invoke return an error" says no.
//
// That is exactly how a corrupted User-Agent header went unnoticed:
// every fetch of a site that refuses Go's default answered 403, the
// model said "that site blocked me", and the only record anywhere was
// the conversation itself.

// logCapture collects records so a test can assert on what an operator
// would actually see.
type logCapture struct {
	strings.Builder
}

func (c *logCapture) Write(p []byte) (int, error) { return c.Builder.Write(p) }

func newLoggedAgent(t *testing.T, exitCode int, stdout, stderr string) (*Agent, *logCapture) {
	t.Helper()
	store := newMemoryStoreForTest(t)
	eng := policy.NewEngine(store, slog.New(slog.DiscardHandler))
	// Allow everything: this test is about what gets LOGGED when a
	// tool fails on its own terms, not about the gate in front of it.
	eng.SetDefaults([]types.PolicyRule{{
		ID: "test-allow-all", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 1,
	}})

	reg := newTestCatalogue()
	if err := reg.Register(&types.ToolDef{
		Name: "fetch_url", Path: BuiltinScheme + "fetch_url", RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}
	b := newTestDispatcher()
	if err := b.Register("fetch_url", func(context.Context, map[string]string) ([]byte, int, error) {
		// A tool that RUNS and fails: non-zero exit, complaint on
		// stderr, and err nil. This is the shape that was invisible.
		if exitCode != 0 {
			return []byte(stdout), exitCode, errors.New(stderr)
		}
		return []byte(stdout), 0, nil
	}); err != nil {
		t.Fatal(err)
	}

	cap := &logCapture{}
	e := NewExecutor(reg, eng, nil, ExecutorConfig{}, slog.New(slog.DiscardHandler))
	e.SetBuiltins(b)

	a, err := NewAgent(AgentConfig{
		Provider: NewMockProvider(),
		Executor: e,
		Logger:   slog.New(slog.NewTextHandler(cap, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, cap
}

func TestAFailedToolCallReachesTheOperatorLog(t *testing.T) {
	a, logs := newLoggedAgent(t, 1, "", "fetch_url: HTTP 403: forbidden")

	_, _, err := a.runToolCall(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice"}, TurnID: "turn-7",
		Budget: mkBudget(t, BudgetCaps{}),
	}, ToolCall{ID: "c1", Name: "fetch_url", Arguments: `{"url":"https://example.invalid/"}`})
	if err != nil {
		t.Fatalf("the turn itself should survive a tool failure: %v", err)
	}

	got := logs.String()
	if !strings.Contains(got, "tool call failed") {
		t.Fatalf("nothing was logged for a failed tool call:\n%s", got)
	}
	for _, want := range []string{"fetch_url", "turn-7", "403"} {
		if !strings.Contains(got, want) {
			t.Errorf("the log line does not name %q:\n%s", want, got)
		}
	}
	// WARN, not ERROR: the model reads the failure and answers anyway,
	// so a routine 404 must not read like an outage.
	if strings.Contains(got, "level=ERROR") {
		t.Errorf("a tool failure logged at ERROR:\n%s", got)
	}
}

// Arguments can carry secrets — a URL with a token in the query string
// is the obvious one — and the line exists to say what failed, which
// the tool name and the first line of the complaint already do.
func TestToolFailureLogDoesNotCarryArguments(t *testing.T) {
	a, logs := newLoggedAgent(t, 1, "", "refused")

	_, _, _ = a.runToolCall(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice"}, TurnID: "turn-8",
		Budget: mkBudget(t, BudgetCaps{}),
	}, ToolCall{
		ID: "c1", Name: "fetch_url",
		Arguments: `{"url":"https://example.invalid/?access_token=SUPERSECRETVALUE"}`,
	})

	if strings.Contains(logs.String(), "SUPERSECRETVALUE") {
		t.Errorf("the failure log leaked a tool argument:\n%s", logs.String())
	}
}

// An error page is mostly markup. Logging one whole would push
// everything around it out of a scrollback while saying nothing the
// first line did not.
func TestToolFailureDetailIsBounded(t *testing.T) {
	huge := "<!DOCTYPE html>\n" + strings.Repeat("<div class=\"error\">forbidden</div>\n", 400)
	a, logs := newLoggedAgent(t, 1, "", huge)

	_, _, _ = a.runToolCall(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice"}, TurnID: "turn-9",
		Budget: mkBudget(t, BudgetCaps{}),
	}, ToolCall{ID: "c1", Name: "fetch_url", Arguments: `{"url":"https://example.invalid/"}`})

	if n := len(logs.String()); n > 4*toolFailureDetailMax {
		t.Errorf("one failure produced %d bytes of log; the detail bound is not holding", n)
	}
}

// The turn is not failed by a tool failure, and a SUCCESSFUL call must
// stay quiet — a warning on every ordinary tool call would train
// everyone to ignore the channel this uses.
func TestASuccessfulToolCallLogsNothing(t *testing.T) {
	a, logs := newLoggedAgent(t, 0, `{"body":"fine"}`, "")

	_, _, err := a.runToolCall(context.Background(), ProcessMessageRequest{
		Claims: &types.Claims{UserID: "alice"}, TurnID: "turn-10",
		Budget: mkBudget(t, BudgetCaps{}),
	}, ToolCall{ID: "c1", Name: "fetch_url", Arguments: `{"url":"https://example.invalid/"}`})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "tool call failed") {
		t.Errorf("a successful tool call produced a failure line:\n%s", logs.String())
	}
}
