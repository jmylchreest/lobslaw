package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// The gate's own tests prove the queueing logic. These prove it is
// actually wired into the request path — the wiring is where a
// correct component quietly does nothing.

// overlapProvider reports whether two turns were ever inside the LLM
// call at the same time, which is exactly the window that used to
// interleave transcripts.
type overlapProvider struct {
	delay time.Duration

	mu       sync.Mutex
	inFlight int
	overlaps int
	calls    int
	prompts  []string
}

func (p *overlapProvider) Chat(_ context.Context, req compute.ChatRequest) (*compute.ChatResponse, error) {
	p.mu.Lock()
	p.inFlight++
	if p.inFlight > 1 {
		p.overlaps++
	}
	p.calls++
	// Record the user's message so folding can be checked.
	for _, m := range req.Messages {
		if m.Role == "user" {
			p.prompts = append(p.prompts, m.Content)
		}
	}
	p.mu.Unlock()

	time.Sleep(p.delay)

	p.mu.Lock()
	p.inFlight--
	p.mu.Unlock()
	return &compute.ChatResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *overlapProvider) stats() (calls, overlaps int, prompts []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.overlaps, append([]string(nil), p.prompts...)
}

func agentWithProvider(t *testing.T, p compute.LLMProvider) *compute.Agent {
	t.Helper()
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: p})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func tgUpdateJSON(updateID, chatID int64, text string) string {
	b, _ := json.Marshal(map[string]any{
		"update_id": updateID,
		"message": map[string]any{
			"message_id": updateID,
			"text":       text,
			"chat":       map[string]any{"id": chatID},
			"from":       map[string]any{"id": 42, "username": "alice"},
		},
	})
	return string(b)
}

// Two webhook updates for the same chat, delivered concurrently as
// net/http would. Without the gate both turns run at once.
func TestTelegramSerialisesConcurrentUpdatesForOneChat(t *testing.T) {
	t.Parallel()
	prov := &overlapProvider{delay: 60 * time.Millisecond}
	h := newTGHarness(t, agentWithProvider(t, prov), TelegramConfig{
		UnknownUserScope: "public",
		QueueMode:        QueueSerial,
	})

	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			postUpdate(t, h.handler, "test-webhook-secret", tgUpdateJSON(int64(i), 555, fmt.Sprintf("msg-%d", i)))
		}(i)
	}
	wg.Wait()

	calls, overlaps, _ := prov.stats()
	if calls != 3 {
		t.Errorf("provider saw %d turns, want 3", calls)
	}
	if overlaps != 0 {
		t.Errorf("%d turns overlapped on one chat — the gate is not on the request path", overlaps)
	}
}

// Different chats must still run at once, or one slow conversation
// serialises the whole bot.
func TestTelegramDoesNotSerialiseAcrossChats(t *testing.T) {
	t.Parallel()
	prov := &overlapProvider{delay: 120 * time.Millisecond}
	h := newTGHarness(t, agentWithProvider(t, prov), TelegramConfig{
		UnknownUserScope: "public",
		QueueMode:        QueueSerial,
	})

	var wg sync.WaitGroup
	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			postUpdate(t, h.handler, "test-webhook-secret", tgUpdateJSON(int64(i), int64(600+i), "hello"))
		}(i)
	}
	wg.Wait()

	calls, overlaps, _ := prov.stats()
	// Asserted on observed overlap, not on the clock. This used to
	// require three 120ms turns to finish inside 300ms, which measures
	// how loaded the machine is as much as whether the gate is
	// per-chat: a contended runner failed it at 338ms while the turns
	// had demonstrably overlapped — serial execution could not have
	// come in under 360ms. inFlight > 1 is the property the test is
	// actually about, and it does not care how slow the box is.
	if overlaps == 0 {
		t.Error("no two chats were ever in flight together — they serialised against each other")
	}
	if calls != 3 {
		t.Errorf("provider saw %d turns, want 3", calls)
	}
}

// Off mode must tell the user, or their message vanishes silently.
func TestTelegramOffModeTellsTheUser(t *testing.T) {
	t.Parallel()
	prov := &overlapProvider{delay: 150 * time.Millisecond}
	h := newTGHarness(t, agentWithProvider(t, prov), TelegramConfig{
		UnknownUserScope: "public",
		QueueMode:        QueueOff,
	})

	var started sync.WaitGroup
	started.Add(1)
	go func() {
		started.Done()
		postUpdate(t, h.handler, "test-webhook-secret", tgUpdateJSON(1, 777, "first"))
	}()
	started.Wait()
	// Let the first turn get into the provider before the second
	// update arrives.
	waitUntil(t, func() bool { c, _, _ := prov.stats(); return c >= 1 }, "first turn never started")

	postUpdate(t, h.handler, "test-webhook-secret", tgUpdateJSON(2, 777, "second"))

	waitUntil(t, func() bool {
		for _, m := range h.sentMessages() {
			if strings.Contains(m.Text, "Still working") {
				return true
			}
		}
		return false
	}, "off mode dropped a message without telling the user")

	if calls, _, _ := prov.stats(); calls != 1 {
		t.Errorf("provider saw %d turns, want 1 — the dropped message still ran", calls)
	}
}

// Debounce must reach the agent as one turn carrying every fragment.
// Folding that loses a fragment is worse than not folding.
func TestTelegramDebounceFoldsIntoOneTurn(t *testing.T) {
	t.Parallel()
	prov := &overlapProvider{delay: 10 * time.Millisecond}
	h := newTGHarness(t, agentWithProvider(t, prov), TelegramConfig{
		UnknownUserScope: "public",
		QueueMode:        QueueDebounce,
		QueueDebounce:    100 * time.Millisecond,
	})

	var wg sync.WaitGroup
	for i, text := range []string{"what is", "the plan", "for today"} {
		wg.Add(1)
		go func(i int, text string) {
			defer wg.Done()
			postUpdate(t, h.handler, "test-webhook-secret", tgUpdateJSON(int64(i+1), 888, text))
		}(i, text)
		time.Sleep(15 * time.Millisecond)
	}
	wg.Wait()

	calls, _, prompts := prov.stats()
	if calls != 1 {
		t.Fatalf("provider saw %d turns, want 1 folded turn (prompts: %v)", calls, prompts)
	}
	joined := strings.Join(prompts, " ")
	for _, want := range []string{"what is", "the plan", "for today"} {
		if !strings.Contains(joined, want) {
			t.Errorf("folded turn lost the fragment %q; the agent saw: %q", want, joined)
		}
	}
}

// The parsed mode must actually reach the handler. A config value
// that silently fails to apply is the failure this whole feature is
// most likely to have.
func TestTelegramHandlerUsesConfiguredMode(t *testing.T) {
	t.Parallel()
	for _, want := range []QueueMode{QueueSerial, QueueLatest, QueueDebounce, QueueOff} {
		h := newTGHarness(t, newAgentFor(t), TelegramConfig{
			UnknownUserScope: "public",
			QueueMode:        want,
		})
		if got := h.handler.gate.Mode(); got != want {
			t.Errorf("handler gate mode = %q, want %q", got, want)
		}
	}
}
