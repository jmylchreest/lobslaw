package compute

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestSpecialistCannotWidenMemoryWithOwnerClaims(t *testing.T) {
	t.Parallel()
	a := &Agent{}
	id := a.TurnIdentityFor(ProcessMessageRequest{
		BotID: "worker", Bot: &BotProfile{ID: "worker", Owner: "user:alice"},
		Principal: identity.Bot("worker"), Claims: &types.Claims{UserID: "alice", Roles: []string{"operator"}},
	})
	for _, authz := range []CrossOwnerAuthorizer{nil, allowAllCrossOwner{}} {
		audience := ReadAudience(context.Background(), id, authz)
		for _, owner := range []string{"user:alice", "bot:worker", "bot:other"} {
			if audience.AllowsEpisodic(&lobslawv1.EpisodicRecord{Owner: owner}) {
				t.Fatalf("specialist read saved records of %s", owner)
			}
		}
	}
}

func TestSpecialistMemoryToolsRefusedAtDispatch(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"memory_search", "memory_write", "memory_recent", "memory_forget", "memory_correct", "pinned_remember", "pinned_forget", "dream_recap", "dream_nap", "session_read", "session_search", "session_list"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			budget, _ := NewTurnBudget(BudgetCaps{})
			dispatcher := &fakeSkillDispatcher{known: map[string]struct{}{name: {}}, response: &SkillInvokeResult{Stdout: []byte("private")}}
			a := &Agent{cfg: AgentConfig{Skills: dispatcher}}
			bot := &BotProfile{ID: "worker", Tools: []string{name}}
			inv, _, err := a.runToolCall(context.Background(), ProcessMessageRequest{Budget: budget, Bot: bot}, ToolCall{Name: name, Arguments: `{}`})
			if err != nil || inv.Error == "" || dispatcher.calls != 0 {
				t.Fatalf("memory tool ran: %+v %v", inv, err)
			}
			bot.IsCoordinator = true
			if len(bot.FilterTools([]Tool{{Name: name}})) != 1 {
				t.Fatal("coordinator lost memory tools")
			}
		})
	}
}

func TestSpecialistTasksUseOnlyExplicitContextAndFreshTranscript(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodicOnly(t, store, "private", "private-saved-marker private saved diary", nil)
	ingester := newCapturingIngester()
	provider := NewMockProviderFunc(func(req ChatRequest, call int) (MockResponse, error) {
		var content string
		for _, m := range req.Messages {
			content += m.Content
		}
		if strings.Contains(content, "private-saved-marker") {
			t.Error("saved memory reached specialist")
		}
		if call == 0 && !strings.Contains(content, "explicit-task-context") {
			t.Error("coordinator context missing")
		}
		if call > 0 && (strings.Contains(content, "explicit-task-context") || strings.Contains(content, "first-task-result")) {
			t.Error("task working memory survived into next task")
		}
		return MockResponse{Content: "first-task-result", FinishReason: "stop"}, nil
	})
	a, err := NewAgent(AgentConfig{
		Provider: provider, EpisodicIngester: ingester,
		Soul: func() *types.SoulConfig { return &types.SoulConfig{Name: "baseline"} },
		PinnedProvider: func(string, string) promptgen.PinnedBlocks {
			t.Error("specialist requested saved pinned memories")
			return promptgen.PinnedBlocks{}
		},
		ContextEngine: NewContextEngine(ContextEngineConfig{Store: store, CrossOwner: allowAllCrossOwner{}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerCtx := turn.WithIdentity(context.Background(), turn.Identity{Principal: identity.User("alice")})
	if got := a.cfg.ContextEngine.Assemble(ownerCtx, "private saved diary").Rendered(); !strings.Contains(got, "private-saved-marker") {
		t.Fatal("test fixture not recallable by coordinator")
	}
	for _, message := range []string{"explicit-task-context private saved query", "second task"} {
		budget, _ := NewTurnBudget(BudgetCaps{})
		_, err := a.RunToolCallLoop(context.Background(), ProcessMessageRequest{
			Message: message, Budget: budget, BotID: "worker", Bot: &BotProfile{ID: "worker", Owner: "user:alice"},
			Claims: &types.Claims{UserID: "alice"}, Principal: identity.Bot("worker"),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-ingester.got:
		t.Fatal("task working memory persisted to bot diary")
	default:
	}
	// Independently exercise the passive recall audience with inherited claims.
	ctx := turn.WithIdentity(context.Background(), a.TurnIdentityFor(ProcessMessageRequest{Bot: &BotProfile{ID: "worker"}, BotID: "worker", Claims: &types.Claims{UserID: "alice"}}))
	if got := a.cfg.ContextEngine.Assemble(ctx, "private saved diary").Rendered(); strings.Contains(got, "private-saved-marker") {
		t.Fatal("passive recall bypassed specialist boundary")
	}
}

func TestSpecialistDoesNotPersistWorkingMemory(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ingester := newCapturingIngester()
		a := &Agent{cfg: AgentConfig{EpisodicIngester: ingester}}
		a.maybeIngestTurn(context.Background(), ProcessMessageRequest{Bot: &BotProfile{ID: "worker"}}, "private task result", nil)
		synctest.Wait()
		select {
		case <-ingester.got:
			t.Fatal("task working memory persisted")
		default:
		}
	})
}
