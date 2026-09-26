package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

type reviewFake struct {
	row       LearnedReview
	decisions int
	approved  bool
}

func (r *reviewFake) List(context.Context, *types.Claims) ([]LearnedReview, error) {
	return []LearnedReview{r.row}, nil
}
func (r *reviewFake) Get(_ context.Context, c *types.Claims, id string) (LearnedReview, error) {
	if c.UserID != "tg-1" || id != r.row.ID {
		return LearnedReview{}, fmt.Errorf("not owned")
	}
	return r.row, nil
}
func (r *reviewFake) Decide(_ context.Context, c *types.Claims, id string, revision uint64, digest string, approve bool) (string, error) {
	if c.UserID != "tg-1" || id != r.row.ID || revision != r.row.Revision || digest != r.row.Digest {
		return "", fmt.Errorf("proposal changed")
	}
	r.decisions++
	r.approved = approve
	return "Decision recorded", nil
}

func reviewTelegram(t *testing.T, prompts Prompts) (*TelegramHandler, *reviewFake, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.URL.Path)
		mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	fake := &reviewFake{row: LearnedReview{ID: "skill:tidy", Name: "tidy", Body: "full instructions", Revision: 3, Digest: "content-digest"}}
	h := &TelegramHandler{cfg: TelegramConfig{Prompts: prompts, Learned: fake, CommandAuthorizer: fakeAuthz{allow: true}, UserIDScopes: map[int64]string{1: "owner", 2: "guest"}}, log: discardLogger(), base: srv.URL, client: srv.Client()}
	return h, fake, &calls
}

func reviewPrompt(t *testing.T, h *TelegramHandler) (*Prompt, *tgCallbackQuery) {
	t.Helper()
	raw, err := json.Marshal(learnedTarget{ID: "skill:tidy", Revision: 3, Digest: "content-digest", UserID: "tg-1"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := h.cfg.Prompts.Create(NewPrompt{Action: learnedReviewAction, Resource: string(raw), Channel: "telegram", ChannelID: "1", RaisedFor: "tg-1", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	q := &tgCallbackQuery{ID: "tap", Data: "learned:approve:" + p.ID, From: &tgUser{ID: 1}, Message: &tgMessage{Chat: tgChat{ID: 1, Type: "private"}}}
	return p, q
}

func TestLearnedDecisionSurvivesPromptStorage(t *testing.T) {
	eachPromptImpl(t, func(t *testing.T, registry Prompts) {
		h, fake, _ := reviewTelegram(t, registry)
		p, q := reviewPrompt(t, h)
		h.handleCallbackQuery(t.Context(), q)
		if fake.decisions != 1 || !fake.approved {
			t.Fatal("approval not applied")
		}
		h.handleCallbackQuery(t.Context(), q)
		if fake.decisions != 1 {
			t.Fatal("repeated tap applied twice")
		}
		got, err := registry.Get(p.ID)
		if err != nil || got.Decision != PromptApproved {
			t.Fatalf("prompt: %v %v", got, err)
		}
	})
}

func TestLearnedCallbackGuards(t *testing.T) {
	cases := []string{"other-user", "other-chat", "group", "policy-revoked", "changed", "expired", "later", "deny", "always"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			registry := NewPromptRegistry()
			h, fake, _ := reviewTelegram(t, registry)
			p, q := reviewPrompt(t, h)
			switch name {
			case "other-user":
				q.From.ID = 2
			case "other-chat":
				q.Message.Chat.ID = 2
			case "group":
				q.Message.Chat.Type = "supergroup"
			case "policy-revoked":
				h.cfg.CommandAuthorizer = fakeAuthz{allow: false}
			case "changed":
				fake.row.Revision++
			case "expired":
				registry.mu.Lock()
				registry.prompts[p.ID].ExpiresAt = time.Now().Add(-time.Minute)
				registry.mu.Unlock()
			case "later":
				q.Data = "learned:later:" + p.ID
			case "deny":
				q.Data = "learned:deny:" + p.ID
			case "always":
				q.Data = "learned:approve-always:" + p.ID
			}
			h.handleCallbackQuery(t.Context(), q)
			want := 0
			if name == "deny" {
				want = 1
			}
			if fake.decisions != want || fake.approved {
				t.Fatalf("unexpected mutation: %+v", fake)
			}
		})
	}
}

func TestLearnedLongReviewIsCompleteDocument(t *testing.T) {
	t.Parallel()
	h, fake, calls := reviewTelegram(t, NewPromptRegistry())
	fake.row.Body = strings.Repeat("instruction\n", 1000)
	if err := h.showLearned(t.Context(), 1, &types.Claims{UserID: "tg-1"}, fake.row.ID); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || !strings.HasSuffix((*calls)[0], "/sendDocument") || !strings.HasSuffix((*calls)[1], "/sendMessage") {
		t.Fatalf("delivery order: %v", *calls)
	}
}

func TestLearnedDeliveryFailureOffersNoApproval(t *testing.T) {
	t.Parallel()
	h, fake, _ := reviewTelegram(t, NewPromptRegistry())
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++; writeJSON(w, map[string]any{"ok": false}) }))
	defer srv.Close()
	h.base = srv.URL
	h.client = srv.Client()
	if err := h.showLearned(t.Context(), 1, &types.Claims{UserID: "tg-1"}, fake.row.ID); err == nil {
		t.Fatal("delivery failure hidden")
	}
	if calls != 1 {
		t.Fatalf("offered buttons after failed delivery: %d calls", calls)
	}
}

func TestLearnedCommandIsPrivateAndBypassesAgent(t *testing.T) {
	t.Parallel()
	h, _, calls := reviewTelegram(t, NewPromptRegistry())
	h.commands = NewCommandSet(fakeAuthz{allow: true}, discardLogger())
	h.registerLearnedCommand()
	req := CommandRequest{Claims: &types.Claims{UserID: "tg-1"}, Session: SessionRef{Channel: "telegram", ChannelID: "1", UserID: "tg-1"}, Shared: true, Name: "learned"}
	if got := h.commands.Dispatch(t.Context(), req); !strings.Contains(got, "direct message") {
		t.Fatalf("shared command: %s", got)
	}
	if len(*calls) != 0 {
		t.Fatal("group request exposed the queue")
	}
	req.Shared = false
	if !h.handleCommand(t.Context(), 1, "/learned", req) {
		t.Fatal("command fell through to the agent")
	}
	if len(*calls) == 0 {
		t.Fatal("queue not delivered")
	}
}

func TestLearnedPromptCannotMintGenericApprovalGrant(t *testing.T) {
	t.Parallel()
	h, fake, _ := reviewTelegram(t, NewPromptRegistry())
	p, q := reviewPrompt(t, h)
	q.Data = "prompt:approve-always:" + p.ID
	h.handleCallbackQuery(t.Context(), q)
	current, err := h.cfg.Prompts.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Decision != PromptPending || fake.decisions != 0 {
		t.Fatal("generic confirmation consumed a skill review")
	}
}
