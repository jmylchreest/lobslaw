package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// tgPromptHarness is a variant of tgServerHarness that captures the
// method name + full body of every Bot API call so tests can assert
// on sendMessage (with reply_markup) and answerCallbackQuery separately.
type tgPromptHarness struct {
	mu       sync.Mutex
	api      *httptest.Server
	calls    []tgAPICall
	handler  *TelegramHandler
	registry *PromptRegistry
}

type tgAPICall struct {
	Method string
	Body   map[string]any
}

func newTGPromptHarness(t *testing.T, agent *compute.Agent, cfg TelegramConfig) *tgPromptHarness {
	t.Helper()
	h := &tgPromptHarness{registry: NewPromptRegistry()}
	h.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		parts := strings.Split(r.URL.Path, "/")
		method := parts[len(parts)-1]
		h.mu.Lock()
		h.calls = append(h.calls, tgAPICall{Method: method, Body: body})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(h.api.Close)

	if cfg.BotToken == "" {
		cfg.BotToken = "test-bot-token"
	}
	if cfg.WebhookSecret == "" {
		cfg.WebhookSecret = "test-webhook-secret"
	}
	cfg.APIBase = h.api.URL
	cfg.Prompts = h.registry
	cfg.ConfirmationTTL = time.Minute

	handler, err := NewTelegramHandler(cfg, agent)
	if err != nil {
		t.Fatal(err)
	}
	h.handler = handler
	return h
}

func (h *tgPromptHarness) capturedCalls() []tgAPICall {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]tgAPICall, len(h.calls))
	copy(out, h.calls)
	return out
}

// TestTelegramSendConfirmationKeyboardRegistersAndPostsKeyboard — when
// the bot would prompt, it MUST register a Prompt in the registry
// AND send a sendMessage with reply_markup.inline_keyboard carrying
// the prompt:approve:<id> / prompt:deny:<id> callback_data.
func TestTelegramSendConfirmationKeyboardRegistersAndPostsKeyboard(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	budget, _ := compute.NewTurnBudget(compute.BudgetCaps{})
	h.handler.sendConfirmationKeyboard(
		555,
		compute.ProcessMessageRequest{TurnID: "turn-42", Budget: budget},
		&compute.ProcessMessageResponse{ConfirmationReason: "run this scary thing?"},
	)

	calls := h.capturedCalls()
	if len(calls) != 1 || calls[0].Method != "sendMessage" {
		t.Fatalf("want 1 sendMessage call; got %+v", calls)
	}

	// reply_markup.inline_keyboard must be present with two buttons.
	markup, _ := calls[0].Body["reply_markup"].(map[string]any)
	if markup == nil {
		t.Fatalf("reply_markup missing: %+v", calls[0].Body)
	}
	rows, _ := markup["inline_keyboard"].([]any)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row; got %+v", rows)
	}
	firstRow, _ := rows[0].([]any)
	if len(firstRow) != 2 {
		t.Fatalf("row should have 2 buttons; got %d", len(firstRow))
	}

	// Each button's callback_data must be "prompt:<verb>:<id>"
	// where id matches a newly-registered prompt.
	var promptID string
	for _, btn := range firstRow {
		b, _ := btn.(map[string]any)
		cd, _ := b["callback_data"].(string)
		parts := strings.SplitN(cd, ":", 3)
		if len(parts) != 3 || parts[0] != "prompt" {
			t.Errorf("callback_data shape: %q", cd)
			continue
		}
		if promptID == "" {
			promptID = parts[2]
		} else if promptID != parts[2] {
			t.Errorf("approve and deny buttons must share the same id; got %q vs %q", promptID, parts[2])
		}
	}
	if promptID == "" {
		t.Fatal("no prompt id extracted from keyboard")
	}
	// Registry must know about the id.
	p, err := h.registry.Get(promptID)
	if err != nil {
		t.Fatalf("registry doesn't know the prompt: %v", err)
	}
	if p.Reason != "run this scary thing?" || p.TurnID != "turn-42" || p.Channel != "telegram" {
		t.Errorf("registry entry mismatch: %+v", p)
	}
}

// TestTelegramCallbackApproveResolves — a Telegram inline-keyboard
// tap fires a callback_query webhook update; the handler parses the
// verb+id from callback_data and resolves the prompt.
func TestTelegramCallbackApproveResolves(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	p, _ := h.registry.Create("turn-x", "reason", "telegram", time.Minute)

	update := `{
		"update_id": 700,
		"callback_query": {
			"id": "cb-001",
			"from": {"id": 1, "username": "u"},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:approve:` + p.ID + `"
		}
	}`
	rec := postUpdate(t, h.handler, "test-webhook-secret", update)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback webhook should 200; got %d", rec.Code)
	}

	// Registry reflects the approval.
	snap, _ := h.registry.Get(p.ID)
	if snap.Decision != PromptApproved {
		t.Errorf("decision: %s", snap.Decision)
	}

	// Both answerCallbackQuery (ack) and sendMessage (text confirm)
	// should have fired.
	calls := h.capturedCalls()
	var sawAck, sawSend bool
	for _, c := range calls {
		if c.Method == "answerCallbackQuery" {
			sawAck = true
			if id, _ := c.Body["callback_query_id"].(string); id != "cb-001" {
				t.Errorf("ack id: %q", id)
			}
		}
		if c.Method == "sendMessage" {
			sawSend = true
		}
	}
	if !sawAck {
		t.Error("answerCallbackQuery not sent")
	}
	if !sawSend {
		t.Error("confirmation sendMessage not sent")
	}
}

func TestTelegramCallbackDenyResolves(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	p, _ := h.registry.Create("turn-x", "reason", "telegram", time.Minute)

	update := `{
		"update_id": 701,
		"callback_query": {
			"id": "cb-002",
			"from": {"id": 1},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:deny:` + p.ID + `"
		}
	}`
	_ = postUpdate(t, h.handler, "test-webhook-secret", update)

	snap, _ := h.registry.Get(p.ID)
	if snap.Decision != PromptDenied {
		t.Errorf("decision: %s", snap.Decision)
	}
}

// TestTelegramCallbackUnknownDataIgnored — forward-compat: a
// callback_data tag the handler doesn't recognise is acked + logged
// but must not crash or touch the registry.
func TestTelegramCallbackUnknownDataIgnored(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	update := `{
		"update_id": 702,
		"callback_query": {
			"id": "cb-003",
			"from": {"id": 1},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "some-future-button:click"
		}
	}`
	rec := postUpdate(t, h.handler, "test-webhook-secret", update)
	if rec.Code != http.StatusOK {
		t.Errorf("unknown callback_data should still 200; got %d", rec.Code)
	}
	// At least the ack should have fired.
	var sawAck bool
	for _, c := range h.capturedCalls() {
		if c.Method == "answerCallbackQuery" {
			sawAck = true
		}
	}
	if !sawAck {
		t.Error("answerCallbackQuery must always fire")
	}
}

// TestTelegramCallbackMissingPromptReportsGracefully — user taps an
// old button after the prompt expired / was reaped. The bot should
// still ack and send a message explaining it's gone, not crash.
func TestTelegramCallbackMissingPromptReportsGracefully(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	update := `{
		"update_id": 703,
		"callback_query": {
			"id": "cb-004",
			"from": {"id": 1},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:approve:nonexistent"
		}
	}`
	rec := postUpdate(t, h.handler, "test-webhook-secret", update)
	if rec.Code != http.StatusOK {
		t.Errorf("missing prompt should still 200; got %d", rec.Code)
	}
	// Find the sendMessage — text should be a graceful error.
	var sendText string
	for _, c := range h.capturedCalls() {
		if c.Method == "sendMessage" {
			sendText, _ = c.Body["text"].(string)
		}
	}
	if sendText == "" {
		t.Fatal("expected a sendMessage reply")
	}
	if !strings.Contains(strings.ToLower(sendText), "no longer exists") {
		t.Errorf("graceful-missing text: %q", sendText)
	}
}

// TestTelegramCallbackDoubleResolveReportsGracefully — user taps
// approve on a prompt they already resolved. Bot acks + reports.
func TestTelegramCallbackDoubleResolveReportsGracefully(t *testing.T) {
	t.Parallel()
	h := newTGPromptHarness(t, newAgentFor(t), TelegramConfig{UnknownUserScope: "public"})

	p, _ := h.registry.Create("t", "r", "telegram", time.Minute)
	_ = h.registry.Resolve(p.ID, PromptApproved)

	update := `{
		"update_id": 704,
		"callback_query": {
			"id": "cb-005",
			"from": {"id": 1},
			"message": {"message_id": 2, "chat": {"id": 99, "type": "private"}, "date": 0},
			"data": "prompt:approve:` + p.ID + `"
		}
	}`
	_ = postUpdate(t, h.handler, "test-webhook-secret", update)

	var sendText string
	for _, c := range h.capturedCalls() {
		if c.Method == "sendMessage" {
			sendText, _ = c.Body["text"].(string)
		}
	}
	if !strings.Contains(strings.ToLower(sendText), "already resolved") {
		t.Errorf("double-resolve text: %q", sendText)
	}
}
