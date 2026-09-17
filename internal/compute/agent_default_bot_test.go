package compute

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A turn that names no bot runs as the default team's coordinator.
//
// Bots were a console-only feature by accident: five code paths build
// a turn and only the console ever set BotID, so Telegram, Slack, REST
// and the inbound webhook all ran the pre-bot assistant. You could
// build a team in the browser, message Telegram, and reach somebody
// who had never heard of them.
func TestATurnWithNoBotRunsAsTheCoordinator(t *testing.T) {
	t.Parallel()

	coordinator := &BotProfile{
		ID:            "coordinator",
		DisplayName:   "Coordinator",
		IsCoordinator: true,
		Tools:         []string{"notify"},
	}
	a := &Agent{cfg: AgentConfig{
		Logger:     discardLogger(),
		DefaultBot: func(context.Context) (*BotProfile, error) { return coordinator, nil },
	}}

	req := &ProcessMessageRequest{Message: "hello from telegram"}
	a.resolveDefaultBot(context.Background(), req)
	if err := a.fillDefaults(context.Background(), req); err != nil {
		t.Fatalf("fillDefaults: %v", err)
	}
	if req.BotID != "coordinator" {
		t.Errorf("BotID = %q, want the coordinator", req.BotID)
	}
	if req.Bot == nil {
		t.Fatal("no profile resolved, so the tool filter and soul lookup both miss")
	}

	// And, crucially, the CONTEXT IDENTITY — which is what the bot
	// tools actually read.
	//
	// The first version of this test stopped at req.BotID and passed
	// while the feature was broken: the identity was built before the
	// resolution ran, so the turn got the coordinator's soul and tool
	// list and every bot tool refused it with "no bot is taking this
	// turn". Asserting the field the caller sets rather than the one
	// the callee reads is how a test agrees with the code and both are
	// wrong together.
	id := a.TurnIdentityFor(*req)
	if id.BotID != "coordinator" {
		t.Errorf("identity.BotID = %q — inbox_list, inbox_post and ask_bot will all refuse this turn", id.BotID)
	}
}

// An explicit bot always wins. The console names one per room, and a
// default that overrode it would make every room the coordinator.
func TestAnExplicitBotIsNotOverriddenByTheDefault(t *testing.T) {
	t.Parallel()

	a := &Agent{cfg: AgentConfig{
		Logger: discardLogger(),
		DefaultBot: func(context.Context) (*BotProfile, error) {
			return &BotProfile{ID: "coordinator"}, nil
		},
	}}
	req := &ProcessMessageRequest{Message: "hi", BotID: "devops"}
	a.resolveDefaultBot(context.Background(), req)
	if err := a.fillDefaults(context.Background(), req); err != nil {
		t.Fatalf("fillDefaults: %v", err)
	}
	if req.BotID != "devops" {
		t.Errorf("BotID = %q, want the bot the caller asked for", req.BotID)
	}
}

// A registry that cannot be reached must not silence the channel.
//
// The failure being avoided is an unanswered Telegram message;
// refusing the turn because the coordinator lookup failed is a worse
// version of the same thing.
func TestAFailedCoordinatorLookupStillAnswers(t *testing.T) {
	t.Parallel()

	a := &Agent{cfg: AgentConfig{
		Logger: discardLogger(),
		DefaultBot: func(context.Context) (*BotProfile, error) {
			return nil, errors.New("raft: not leader")
		},
	}}
	req := &ProcessMessageRequest{Message: "hello"}
	a.resolveDefaultBot(context.Background(), req)
	if err := a.fillDefaults(context.Background(), req); err != nil {
		t.Fatalf("a failed lookup failed the turn: %v", err)
	}
	if req.Bot != nil || req.BotID != "" {
		t.Error("a failed lookup left a bot set")
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// toolSeeingLoop records the identity a tool would see mid-turn.
type identityProbe struct{ botID string }

// A bot tool called during a channel turn must find a bot.
//
// This is the assertion that would have caught the real failure. The
// unit tests above check the request; the tools check the CONTEXT, and
// for a while those disagreed: a Telegram turn ran with the
// coordinator's soul and tool list while inbox_list, inbox_post and
// ask_bot all refused it with "no bot is taking this turn". The
// coordinator could talk and could not delegate.
func TestABotToolFindsItsBotOnAChannelTurn(t *testing.T) {
	t.Parallel()

	probe := &identityProbe{}
	a := &Agent{cfg: AgentConfig{
		Logger: discardLogger(),
		DefaultBot: func(context.Context) (*BotProfile, error) {
			return &BotProfile{ID: "coordinator", IsCoordinator: true}, nil
		},
	}}

	// A turn arriving from a channel: claims for a person, no bot named.
	req := ProcessMessageRequest{
		Message: "delegate this",
		Claims:  &types.Claims{UserID: "user:james"},
		Channel: "telegram", ChannelID: "5053517285",
	}

	// Exactly what RunToolCallLoop does, in order.
	ctx := context.Background()
	a.resolveDefaultBot(ctx, &req)
	ctx = turn.WithIdentity(ctx, a.TurnIdentityFor(req))

	// And exactly what a bot tool does with it.
	id, ok := turn.IdentityFrom(ctx)
	if !ok {
		t.Fatal("no identity in the turn context")
	}
	probe.botID = id.BotID
	if probe.botID == "" {
		t.Fatal("a bot tool would refuse this turn: no bot is taking it")
	}
	if probe.botID != "coordinator" {
		t.Errorf("tools see bot %q, want the coordinator", probe.botID)
	}
	// The person is still the person — the bot is doing the work, not
	// becoming the caller.
	if id.Principal.IsBot() {
		t.Error("the turn took the bot's principal; ownership and recall would follow the bot, not James")
	}
}

// A resumed turn runs on the bot's caps, not the node's.
//
// Tighten had one call site. The resume path resolved the bot and
// stopped, though its comment claimed the ordering matched — so a turn
// continuing after an approval ran uncapped, on the single path where
// a guarded tool is about to run having just been authorised.
func TestAResumedTurnKeepsTheBotsCaps(t *testing.T) {
	t.Parallel()

	restricted := &BotProfile{ID: "devops", Caps: BudgetCaps{MaxToolCalls: 2}}
	// Both entry points, through the real functions rather than a
	// re-implementation of them.
	for _, entry := range []string{"run", "resume"} {
		t.Run(entry, func(t *testing.T) {
			budget, err := NewTurnBudget(BudgetCaps{MaxToolCalls: 30})
			if err != nil {
				t.Fatalf("NewTurnBudget: %v", err)
			}
			a := &Agent{cfg: AgentConfig{
				Provider: NewMockProvider(MockResponse{Content: "ok"}),
				Logger:   discardLogger(),
				DefaultBot: func(context.Context) (*BotProfile, error) {
					return restricted, nil
				},
			}}
			req := ProcessMessageRequest{Message: "go", Budget: budget}

			ctx := context.Background()
			switch entry {
			case "run":
				_, _ = a.RunToolCallLoop(ctx, req)
			case "resume":
				_, _ = a.ResumeFromConfirmation(ctx, req,
					[]Message{{Role: "user", Content: "go"}})
			}

			if got := budget.Caps().MaxToolCalls; got != 2 {
				t.Errorf("%s: MaxToolCalls = %d, want the bot's 2 — "+
					"this turn ran on the node's allowance", entry, got)
			}
		})
	}
}
