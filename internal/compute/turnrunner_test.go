package compute

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// recordingLoop captures the request the runner built, which is where
// every invariant worth asserting ends up: the tool list the model
// will be shown, the budget it draws on, the claims it runs as.
type recordingLoop struct {
	mu   sync.Mutex
	reqs []ProcessMessageRequest
	err  error
}

func (l *recordingLoop) RunToolCallLoop(_ context.Context, req ProcessMessageRequest) (*ProcessMessageResponse, error) {
	l.mu.Lock()
	l.reqs = append(l.reqs, req)
	l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	return &ProcessMessageResponse{Reply: "ok"}, nil
}

func (l *recordingLoop) last() ProcessMessageRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reqs[len(l.reqs)-1]
}

type mapResolver map[string]*BotProfile

func (m mapResolver) ResolveBot(_ context.Context, id string) (*BotProfile, error) {
	p, ok := m[id]
	if !ok {
		return nil, errors.New("no such bot")
	}
	return p, nil
}

func testRunner(t *testing.T, loop TurnLoop, bots BotResolver, caps BudgetCaps) *TurnRunner {
	t.Helper()
	r, err := NewTurnRunner(loop, bots, caps, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}
	return r
}

func toolNames(tools []Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

var nodeTools = []Tool{
	{Name: "web_search"},
	{Name: "shell_command"},
	{Name: "memory_write"},
	{Name: "ask_bot"},
}

// The registry filter is applied by the agent, not by the runner,
// because the agent is the choke point EVERY turn passes through —
// channels included. Asserting it anywhere else would prove it for
// headless turns only, which is the half that matters least: the GUI
// chats to a bot through a channel.
//
// So these go through a real Agent and read the tool list off the
// provider request, which is the last place it exists before the model
// sees it.
func advertisedTools(t *testing.T, bots BotResolver, req TurnRequest) []string {
	t.Helper()
	mock := NewMockProvider(MockResponse{Content: "done"})
	agent, err := NewAgent(AgentConfig{Provider: mock})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	r := testRunner(t, agent, bots, BudgetCaps{})
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	calls := mock.Calls()
	if len(calls) == 0 {
		t.Fatal("the provider was never called")
	}
	return toolNames(calls[0].Tools)
}

// Isolation has to be STRUCTURAL: the marketing bot is not shown
// shell_command, so there is no refusal to argue with and no policy
// decision to get wrong. Asserted on the tool list the turn was built
// with, not on a policy verdict.
func TestBotIsNeverShownAToolOutsideItsAllowlist(t *testing.T) {
	t.Parallel()
	got := advertisedTools(t, mapResolver{
		"marketing": {ID: "marketing", Tools: []string{"web_search", "memory_write"}},
	}, TurnRequest{
		BotID: "marketing", Prompt: "draft a launch post", Origin: "test", OriginID: "1",
		Tools: nodeTools,
	})

	if len(got) != 2 {
		t.Fatalf("advertised tools = %v, want exactly the two allowed", got)
	}
	for _, banned := range []string{"shell_command", "ask_bot"} {
		if slices.Contains(got, banned) {
			t.Errorf("marketing was shown %q", banned)
		}
	}
}

// An empty allowlist means the node's full set, not the empty set. A
// bot created without anyone stating its tools should be as capable as
// the assistant was before it existed; silently muting one presents as
// a broken bot rather than as a decision somebody made.
func TestEmptyAllowlistMeansEverythingNotNothing(t *testing.T) {
	t.Parallel()
	got := advertisedTools(t, mapResolver{"generalist": {ID: "generalist"}}, TurnRequest{
		BotID: "generalist", Prompt: "hello", Origin: "test", OriginID: "1", Tools: nodeTools,
	})
	if len(got) != len(nodeTools) {
		t.Errorf("advertised %v, want the node's full set", got)
	}
}

// The delegation guard. A child reached through ask_bot has ask_bot
// removed from the registry it is built with, so a second hop is
// unexpressible rather than merely disallowed — no depth counter to
// keep enforcing, and a fork bomb is not a thing that can be typed.
func TestWithoutRemovesAToolEvenFromAnUnrestrictedBot(t *testing.T) {
	t.Parallel()
	unrestricted := &BotProfile{ID: "engineering"}
	got := advertisedTools(t, nil, TurnRequest{
		Profile: unrestricted.Without("ask_bot"),
		Prompt:  "answer this", Origin: "ask", OriginID: "1", Tools: nodeTools,
	})

	if slices.Contains(got, "ask_bot") {
		t.Fatal("a delegated child was still shown ask_bot; one hop is not enforced")
	}
	if len(got) != len(nodeTools)-1 {
		t.Errorf("child lost more than the one tool it should have: %v", got)
	}
}

// Subtracting the only tool a bot had must not fall back through the
// empty-means-everything rule and silently re-grant it. That is why
// the denial is a separate list subtracted last, rather than a
// narrowing of the allowlist.
func TestWithoutEmptyingAnAllowlistDoesNotReGrantEverything(t *testing.T) {
	t.Parallel()
	narrow := &BotProfile{ID: "narrow", Tools: []string{"ask_bot"}}
	got := advertisedTools(t, nil, TurnRequest{
		Profile: narrow.Without("ask_bot"),
		Prompt:  "x", Origin: "ask", OriginID: "1", Tools: nodeTools,
	})
	if len(got) != 0 {
		t.Errorf("emptied allowlist re-granted %v", got)
	}
}

// A bot's turns run as its own principal, so memory ownership, policy
// subjects and audit all attribute to the bot rather than to whoever
// happened to boot the node.
func TestBotTurnRunsAsItsOwnPrincipal(t *testing.T) {
	t.Parallel()
	loop := &recordingLoop{}
	r := testRunner(t, loop, mapResolver{"devops": {ID: "devops"}}, BudgetCaps{})

	if _, err := r.Run(context.Background(), TurnRequest{
		BotID: "devops", Prompt: "check the cluster", Origin: "task", OriginID: "1",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	claims := loop.last().Claims
	if claims.UserID != "bot:devops" {
		t.Errorf("claims.UserID = %q, want %q", claims.UserID, "bot:devops")
	}
	if claims.Scope != "bot:devops" {
		t.Errorf("claims.Scope = %q, want %q", claims.Scope, "bot:devops")
	}
}

// Explicit claims win, so a routine somebody scheduled still attributes
// to them for audit even though a bot is doing the work.
func TestExplicitClaimsOverrideTheBotPrincipal(t *testing.T) {
	t.Parallel()
	loop := &recordingLoop{}
	r := testRunner(t, loop, mapResolver{"devops": {ID: "devops"}}, BudgetCaps{})

	if _, err := r.Run(context.Background(), TurnRequest{
		BotID: "devops", Prompt: "x", Origin: "task", OriginID: "1",
		Claims: &types.Claims{UserID: "user:alice", Scope: "scheduler"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := loop.last().Claims.UserID; got != "user:alice" {
		t.Errorf("claims.UserID = %q, want the explicit %q", got, "user:alice")
	}
}

// A bot may tighten the operator's caps and must not escape them. The
// operator's number is the one chosen deliberately; a bot record the
// coordinator wrote is not the place to overrule it.
func TestBotCapsTightenButDoNotEscapeTheNodeCaps(t *testing.T) {
	t.Parallel()
	loop := &recordingLoop{}
	r := testRunner(t, loop, mapResolver{
		"thrifty":   {ID: "thrifty", Caps: BudgetCaps{MaxToolCalls: 3}},
		"ambitious": {ID: "ambitious", Caps: BudgetCaps{MaxToolCalls: 500}},
	}, BudgetCaps{MaxToolCalls: 10})

	spend := func(botID string) int {
		if _, err := r.Run(context.Background(), TurnRequest{
			BotID: botID, Prompt: "x", Origin: "test", OriginID: "1",
		}); err != nil {
			t.Fatalf("Run %s: %v", botID, err)
		}
		budget := loop.last().Budget
		allowed := 0
		for range 1000 {
			if !budget.RecordToolCall().Within {
				break
			}
			allowed++
		}
		return allowed
	}

	if got := spend("thrifty"); got != 3 {
		t.Errorf("thrifty bot spent %d, want its own tighter cap of 3", got)
	}
	if got := spend("ambitious"); got != 10 {
		t.Errorf("ambitious bot spent %d, want the node's cap of 10 — a bot must not raise it", got)
	}
}

// Delegation's bound: a child's budget draws on the parent's, so a
// tree of turns is limited by the one number the root authorised.
func TestReservationBoundsADelegatedTurn(t *testing.T) {
	t.Parallel()
	loop := &recordingLoop{}
	r := testRunner(t, loop, nil, BudgetCaps{MaxToolCalls: 100})
	reservation, _ := NewTurnBudget(BudgetCaps{MaxToolCalls: 2})

	if _, err := r.Run(context.Background(), TurnRequest{
		Prompt: "x", Origin: "ask", OriginID: "1", Reservation: reservation,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	budget := loop.last().Budget
	allowed := 0
	for range 50 {
		if budget.RecordToolCall().Within {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("delegated turn spent %d against a reservation of 2", allowed)
	}
}

func TestRunnerRefusesAnEmptyPrompt(t *testing.T) {
	t.Parallel()
	loop := &recordingLoop{}
	r := testRunner(t, loop, nil, BudgetCaps{})

	_, err := r.Run(context.Background(), TurnRequest{Prompt: "  ", Origin: "task", OriginID: "t1"})
	if err == nil {
		t.Fatal("ran a turn with no prompt; that is a provider call asking the model to answer its own system prompt")
	}
	if !strings.Contains(err.Error(), "t1") {
		t.Errorf("error does not name the record that caused it: %v", err)
	}
	if len(loop.reqs) != 0 {
		t.Error("the loop was entered anyway")
	}
}

// No bot means the node's default assistant — exactly what every turn
// did before bots existed, so the upgrade changes nothing for a
// deployment that never creates one.
func TestNoBotRunsAsTheNodeDefault(t *testing.T) {
	t.Parallel()
	loop := &recordingLoop{}
	r := testRunner(t, loop, nil, BudgetCaps{})

	if _, err := r.Run(context.Background(), TurnRequest{
		Prompt: "hello", Origin: "task", OriginID: "1", Tools: nodeTools,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := loop.last()
	if req.Bot != nil || req.BotID != "" {
		t.Errorf("a turn with no bot carried one: %v / %q", req.Bot, req.BotID)
	}
	if len(req.Tools) != len(nodeTools) {
		t.Errorf("tools were filtered with no bot to filter by: %v", toolNames(req.Tools))
	}
}

// The brief is appended to the operator's soul body, not substituted
// for it. Replacing would let creating a bot silently opt out of house
// style and safety guidance written for all of them.
func TestBotBriefIsAppendedToTheSoulBody(t *testing.T) {
	t.Parallel()
	const body = "Operator baseline: be concise."
	got := appendBotBrief(body, &BotProfile{
		ID: "engineering", DisplayName: "Engineering", Instructions: "You are the engineer for XYZ.",
	})
	if !strings.Contains(got, body) {
		t.Error("the operator's soul body was dropped")
	}
	if !strings.Contains(got, "You are the engineer for XYZ.") {
		t.Error("the bot's brief is missing")
	}
	if !strings.Contains(got, "You are Engineering.") {
		t.Error("the bot is not named to itself")
	}
	if strings.Index(got, body) > strings.Index(got, "engineer for XYZ") {
		t.Error("the brief precedes the baseline; the baseline governs it")
	}
}

func TestNoBriefLeavesTheSoulBodyByteIdentical(t *testing.T) {
	t.Parallel()
	const body = "Operator baseline."
	if got := appendBotBrief(body, nil); got != body {
		t.Errorf("no bot changed the body: %q", got)
	}
	if got := appendBotBrief(body, &BotProfile{ID: "quiet"}); got != body {
		t.Errorf("a bot with no brief changed the body: %q", got)
	}
}

func TestMayMessageIsAnAllowlist(t *testing.T) {
	t.Parallel()
	p := &BotProfile{ID: "marketing", MayMessage: []string{"engineering"}}
	if !p.MayMessageBot("engineering") {
		t.Error("a declared edge was refused")
	}
	if p.MayMessageBot("devops") {
		t.Error("an undeclared edge was allowed; the graph is an allowlist")
	}
	if (&BotProfile{ID: "lonely"}).MayMessageBot("anyone") {
		t.Error("a bot with no declared edges could reach one")
	}
}

// fixedLoop returns one prepared response. recordingLoop cannot be
// used here: it replies "ok" with no Messages, and the thing under
// test is precisely which messages get persisted.
type fixedLoop struct{ resp *ProcessMessageResponse }

func (l *fixedLoop) RunToolCallLoop(_ context.Context, _ ProcessMessageRequest) (*ProcessMessageResponse, error) {
	return l.resp, nil
}

// recordingWriter captures what the runner persists.
type recordingWriter struct {
	channel, channelID, turnID string
	msgs                       []Message
	id                         string
	err                        error
	history                    []Message
	summary                    string
	loadErr                    error
}

func (w *recordingWriter) AppendTurn(_ context.Context, channel, channelID, turnID string, msgs []Message) (string, error) {
	w.channel, w.channelID, w.turnID, w.msgs = channel, channelID, turnID, msgs
	return w.id, w.err
}

func (w *recordingWriter) LoadTurns(_ context.Context, _, _ string, _ int) ([]Message, string, error) {
	return w.history, w.summary, w.loadErr
}

// A headless turn must be written to a session and report where.
//
// It was not. Only the channel path persisted a conversation, so a
// turn started from Telegram was recorded and the identical turn
// started by the inbox drain was not — which left every "what did it
// actually do" link in the console pointing at nothing. The field, the
// route and the UI all existed; the write did not.
func TestHeadlessTurnIsPersistedAndReportsItsSession(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{id: "sess-123"}
	loop := &fixedLoop{resp: &ProcessMessageResponse{
		Reply:    "done",
		Messages: []Message{{Role: "user", Content: "do it"}, {Role: "assistant", Content: "done"}},
	}}
	r, err := NewTurnRunner(loop, nil, BudgetCaps{}, writer,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}

	resp, err := r.Run(context.Background(), TurnRequest{
		Prompt: "do it", Origin: "inbox", OriginID: "item-1",
		Channel: "bot", ChannelID: "devops:inbox:item-1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.SessionID != "sess-123" {
		t.Errorf("SessionID = %q, want the id the writer returned", resp.SessionID)
	}
	if writer.channel != "bot" || writer.channelID != "devops:inbox:item-1" {
		t.Errorf("persisted to %q/%q, want bot/devops:inbox:item-1", writer.channel, writer.channelID)
	}
	if len(writer.msgs) != 2 {
		t.Errorf("persisted %d messages, want the turn's 2", len(writer.msgs))
	}
}

// A store that is down must not lose work that already happened.
//
// The turn has been run and paid for by the time it is written, so a
// failed append is a missing transcript, not a failed task. Returning
// the error here would mark completed work as failed and retry it —
// spending a second provider call to fix a logging problem.
func TestATurnSurvivesAFailedTranscriptWrite(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{err: errors.New("raft: not leader")}
	loop := &fixedLoop{resp: &ProcessMessageResponse{
		Reply:    "done anyway",
		Messages: []Message{{Role: "assistant", Content: "done anyway"}},
	}}
	r, err := NewTurnRunner(loop, nil, BudgetCaps{}, writer,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}

	resp, err := r.Run(context.Background(), TurnRequest{
		Prompt: "do it", Origin: "inbox", OriginID: "item-2",
		Channel: "bot", ChannelID: "devops:inbox:item-2",
	})
	if err != nil {
		t.Fatalf("a failed transcript write failed the whole turn: %v", err)
	}
	if resp.Reply != "done anyway" {
		t.Errorf("Reply = %q, want the work's actual result", resp.Reply)
	}
	if resp.SessionID != "" {
		t.Errorf("SessionID = %q, want empty when nothing was stored", resp.SessionID)
	}
}

func (l *recordingLoop) ResumeFromConfirmation(_ context.Context, _ ProcessMessageRequest, _ []Message) (*ProcessMessageResponse, error) {
	return nil, errors.New("ResumeFromConfirmation: not expected in this test")
}

func (l *fixedLoop) ResumeFromConfirmation(_ context.Context, _ ProcessMessageRequest, _ []Message) (*ProcessMessageResponse, error) {
	return nil, errors.New("ResumeFromConfirmation: not expected in this test")
}

// askingLoop stops once to ask, then succeeds when resumed.
type askingLoop struct {
	resumed  bool
	relaxed  bool
	priorLen int
}

func (l *askingLoop) RunToolCallLoop(_ context.Context, req ProcessMessageRequest) (*ProcessMessageResponse, error) {
	return &ProcessMessageResponse{
		NeedsConfirmation:    true,
		ConfirmationReason:   "shell_command is guarded",
		ConfirmationAction:   "shell",
		ConfirmationResource: "rm -rf /tmp/x",
		ToolCalls:            []ToolInvocation{{ToolName: "glob"}},
		Messages:             []Message{{Role: "user", Content: "do it"}},
	}, nil
}

func (l *askingLoop) ResumeFromConfirmation(ctx context.Context, req ProcessMessageRequest, prior []Message) (*ProcessMessageResponse, error) {
	l.resumed = true
	l.priorLen = len(prior)
	// The caps must be lifted before re-entry, or the resumed half
	// re-trips the same bound the user just authorised past.
	l.relaxed = req.Budget.Caps().MaxToolCalls == 0
	// The approval has to reach the tools, not just the runner.
	// turnApprovalPending rather than turnApproved: the latter SPENDS
	// the approval, so asserting with it would consume the very thing
	// the resumed tool call needs.
	if !turnApprovalPending(ctx) {
		return nil, errors.New("resumed without the turn approval in context")
	}
	return &ProcessMessageResponse{
		Reply:     "done",
		ToolCalls: []ToolInvocation{{ToolName: "shell_command"}},
		Messages:  prior,
	}, nil
}

// A headless turn can now ask the person who started it.
//
// Before, every caller failed closed unconditionally: a turn that
// reached a guarded tool stopped and reported that it had nobody to
// ask, even when the operator was sitting on the other end of an open
// stream. Confirm is how a caller that HAS somebody says so.
func TestATurnCanAskAndCarryOnWhenApproved(t *testing.T) {
	t.Parallel()

	loop := &askingLoop{}
	r, err := NewTurnRunner(loop, nil, BudgetCaps{MaxToolCalls: 3}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}

	var askedReason string
	resp, err := r.Run(context.Background(), TurnRequest{
		Prompt: "do it", Origin: "console", OriginID: "t1",
		Confirm: func(_ context.Context, reason, _, _ string) (bool, error) {
			askedReason = reason
			return true, nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if askedReason != "shell_command is guarded" {
		t.Errorf("the reason put to the user was %q", askedReason)
	}
	if !loop.resumed {
		t.Fatal("approved, but the turn was never resumed")
	}
	if !loop.relaxed {
		t.Error("resumed without relaxing the budget; the approved call re-trips the cap")
	}
	if resp.NeedsConfirmation {
		t.Error("response still reports NeedsConfirmation after approval")
	}
	// The receipt has to cover the whole turn, not just the half that
	// ran after the question — otherwise approving a turn hides what
	// it did beforehand.
	if got := InvokedToolNames(resp.ToolCalls); len(got) != 2 {
		t.Errorf("tool calls = %v, want both halves of the turn", got)
	}
}

// Declining stops the turn and says so plainly.
func TestADeclinedTurnStopsAndSaysWhy(t *testing.T) {
	t.Parallel()

	r, err := NewTurnRunner(&askingLoop{}, nil, BudgetCaps{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}
	resp, err := r.Run(context.Background(), TurnRequest{
		Prompt: "do it", Origin: "console", OriginID: "t2",
		Confirm: func(context.Context, string, string, string) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.NeedsConfirmation {
		t.Error("a declined turn still reports NeedsConfirmation")
	}
	if !strings.Contains(resp.Reply, "declined") {
		t.Errorf("Reply = %q, want it to say the request was declined", resp.Reply)
	}
}

// A prompt channel that breaks is an unanswered question, never a yes.
func TestABrokenPromptChannelIsNotConsent(t *testing.T) {
	t.Parallel()

	loop := &askingLoop{}
	r, err := NewTurnRunner(loop, nil, BudgetCaps{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}
	resp, err := r.Run(context.Background(), TurnRequest{
		Prompt: "do it", Origin: "console", OriginID: "t3",
		Confirm: func(context.Context, string, string, string) (bool, error) {
			return false, errors.New("prompt registry is down")
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if loop.resumed {
		t.Fatal("a failed prompt was treated as approval and the turn carried on")
	}
	if !resp.NeedsConfirmation {
		t.Error("the turn should still report that it is waiting on an answer")
	}
}

// A conversation has to be READ back, not only written.
//
// Persisting a transcript nothing loads gives a bot that answers every
// message as though it were the first. The console showed that as a
// thread vanishing on refresh; the worse half was invisible, because
// the MODEL had no history either and a follow-up landed with no idea
// what it followed.
func TestATurnRepliesWithTheConversationSoFar(t *testing.T) {
	t.Parallel()

	prior := []Message{
		{Role: "user", Content: "my cluster is called nova"},
		{Role: "assistant", Content: "noted"},
	}
	writer := &recordingWriter{id: "s1", history: prior, summary: "earlier: setup talk"}
	loop := &recordingLoop{}
	r, err := NewTurnRunner(loop, nil, BudgetCaps{}, writer,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}

	if _, err := r.Run(context.Background(), TurnRequest{
		Prompt: "what is it called?", Origin: "console", OriginID: "t1",
		Channel: "bot", ChannelID: "coordinator",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := loop.last()
	if len(got.ConversationHistory) != 2 {
		t.Errorf("history = %d messages, want the 2 already on record", len(got.ConversationHistory))
	}
	if got.ConversationSummary != "earlier: setup talk" {
		t.Errorf("summary = %q, want the stored one", got.ConversationSummary)
	}
}

// A turn with no address has no conversation, and must not invent one.
// An inbox item gets its own session per item, which is what keeps an
// independent task independent.
func TestATurnWithNoAddressHasNoHistory(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{history: []Message{{Role: "user", Content: "should not appear"}}}
	loop := &recordingLoop{}
	r, _ := NewTurnRunner(loop, nil, BudgetCaps{}, writer,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := r.Run(context.Background(), TurnRequest{
		Prompt: "do it", Origin: "scheduler", OriginID: "t2",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(loop.last().ConversationHistory) != 0 {
		t.Error("a turn with no channel picked up somebody else's conversation")
	}
}

// A transcript that cannot be read must not take the turn with it.
func TestAFailedHistoryLoadStillAnswers(t *testing.T) {
	t.Parallel()

	writer := &recordingWriter{loadErr: errors.New("raft: not leader")}
	loop := &recordingLoop{}
	r, _ := NewTurnRunner(loop, nil, BudgetCaps{}, writer,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := r.Run(context.Background(), TurnRequest{
		Prompt: "hello", Origin: "console", OriginID: "t3",
		Channel: "bot", ChannelID: "coordinator",
	}); err != nil {
		t.Fatalf("a failed history load failed the turn: %v", err)
	}
}

// Every field the runner accepts has to reach the turn.
//
// RequestedBy was declared on TurnRequest and never copied into the
// agent request, so provenance died silently one hop short of the
// tool that needed it: the queue item held "user:sam", the turn ran,
// the notification went out attributed to nobody. Nothing failed —
// the field was simply dropped.
//
// Asserting the plumbing rather than one field, because this is a
// struct copied by hand and the next addition can be forgotten the
// same way.
func TestTheRunnerPassesItsRequestThrough(t *testing.T) {
	t.Parallel()

	loop := &recordingLoop{}
	r, err := NewTurnRunner(loop, nil, BudgetCaps{}, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewTurnRunner: %v", err)
	}

	if _, err := r.Run(context.Background(), TurnRequest{
		Prompt:       "do it",
		Origin:       "inbox",
		OriginID:     "item-1",
		Channel:      "bot",
		ChannelID:    "research",
		SystemPrompt: "you are research",
		RequestedBy:  "user:sam",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := loop.last()
	for _, c := range []struct{ name, want, have string }{
		{"Message", "do it", got.Message},
		{"Channel", "bot", got.Channel},
		{"ChannelID", "research", got.ChannelID},
		{"SystemPrompt", "you are research", got.SystemPrompt},
		{"RequestedBy", "user:sam", got.RequestedBy},
	} {
		if c.have != c.want {
			t.Errorf("%s = %q, want %q — the runner dropped it", c.name, c.have, c.want)
		}
	}
}

// A bot that is a real team member is told how to talk to a person.
//
// The machinery leaked without it: asked for a status on Telegram, the
// coordinator replied with tool names, a ULID and "the durable route
// is open" — an accurate account of its own plumbing and no use to
// somebody holding a phone.
func TestATeamMemberIsToldHowToReport(t *testing.T) {
	t.Parallel()

	t.Run("a bot with a brief gets it", func(t *testing.T) {
		got := appendBotBrief("Baseline.", &BotProfile{
			ID: "devops", DisplayName: "DevOps", Instructions: "You own the cluster.",
		})
		if !strings.Contains(got, "not writing a log") {
			t.Error("no reporting guidance; the bot will narrate its plumbing")
		}
		if !strings.Contains(got, "You own the cluster.") {
			t.Error("the operator's brief was dropped")
		}
	})

	t.Run("a named bot with no brief still gets it", func(t *testing.T) {
		got := appendBotBrief("Baseline.", &BotProfile{ID: "devops", DisplayName: "DevOps"})
		if !strings.Contains(got, "not writing a log") {
			t.Error("a named team member was left without guidance")
		}
	})

	// The upgrade case. A profile with neither a brief nor a name is
	// the node's own assistant wearing a bot record, and changing how
	// it talks is a behaviour change nobody asked for.
	t.Run("an unconfigured profile leaves the soul untouched", func(t *testing.T) {
		const body = "Operator baseline."
		if got := appendBotBrief(body, &BotProfile{ID: "quiet"}); got != body {
			t.Errorf("an upgraded deployment's assistant changed voice: %q", got)
		}
	})
}
