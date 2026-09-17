package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// handleBotChat serves POST /v1/bots/{id}/messages.
//
// Talking to a SPECIFIC bot, which /v1/messages cannot do — that
// endpoint is the coordinator's, shared with Telegram and Slack, and a
// console that could only reach the coordinator would make every specialist
// something you can configure but not converse with.
//
// Streamed as Server-Sent Events. A bot turn can run tools for a
// minute, and a request that returns nothing until it finishes is
// indistinguishable from one that has hung — which is how somebody
// ends up reloading and starting a second turn.
func (s *Server) handleBotChat(w http.ResponseWriter, r *http.Request, botID string) {
	if s.cfg.Turns == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node cannot run turns")
		return
	}
	if r.Method != http.MethodPost {
		s.jsonErr(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Authenticated here, before the 200 and the SSE headers go out.
	//
	// runBotTurn used to be the first thing that checked, by which
	// point the status line, the stream headers and a `start` event
	// were already on the wire — so an unauthenticated caller got a
	// streamed error instead of a 401, and anything speaking HTTP
	// rather than SSE saw a successful request.
	claims, authErr := s.authenticate(r)
	if authErr != nil {
		s.jsonErr(w, http.StatusUnauthorized, authErr.Error())
		return
	}

	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		s.jsonErr(w, http.StatusBadRequest, "message is required")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, SSE is just a slow JSON response that
		// arrives all at once — worse than admitting it.
		s.jsonErr(w, http.StatusInternalServerError, "this server cannot stream")
		return
	}
	// Clear the write deadline for THIS response only.
	//
	// The server sets WriteTimeout=60s, which is right for every other
	// endpoint and fatal here: it is an absolute deadline from the
	// start of the response, not an inactivity timeout, so the
	// heartbeat below does not extend it. A bot turn against a real
	// model routinely runs past a minute — one that delegates to three
	// specialists took 159s — and every one of those was killed
	// mid-stream. The turn itself completed and was recorded; only the
	// person watching was told "network error", which is the worst
	// possible split: the work happened and the UI said it failed.
	//
	// Scoped to this handler rather than raised globally, so a slow or
	// stuck request anywhere else still hits the server-wide bound.
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		// Not fatal — an unwrapped ResponseWriter in a test has no
		// deadline to clear, and the stream is still correct.
		s.log.Debug("chat: could not clear write deadline", "err", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Reverse proxies buffer by default and would hold the whole
	// stream until the turn ended, which defeats the point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	turnID := ids.New()

	// Three goroutines write to this stream: the heartbeat ticker, the
	// delta hook, and this handler. An http.ResponseWriter is not safe
	// for concurrent use, and interleaved writes corrupt frames rather
	// than merely reordering them.
	//
	// One guarded emitter rather than a mutex the call sites take
	// themselves. The first version did the latter and a mismatched
	// pair — an Unlock whose Lock had been lost in an edit — took the
	// whole node down with "unlock of unlocked mutex" the first time a
	// turn asked for approval. A lock nobody outside this closure can
	// touch cannot be left unbalanced.
	var sseMu sync.Mutex
	emit := func(event string, payload map[string]any) {
		sseMu.Lock()
		defer sseMu.Unlock()
		sendSSE(w, flusher, event, payload)
	}

	emit("start", map[string]any{"bot": botID, "turn_id": turnID})

	// A heartbeat while the turn runs. The console shows it as
	// "working", and it also keeps an idle-timeout proxy from closing a
	// connection that is legitimately quiet for ninety seconds.
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				emit("working", map[string]any{"turn_id": turnID})
			}
		}
	}()

	// The confirmation hook. The person who started this turn is still
	// on the other end of the stream, so the question goes to them:
	// register a prompt, push its id down the wire for the console to
	// render buttons against, and block until they answer or the TTL
	// fires. Previously this path could only report that it was unable
	// to ask, which made every guarded tool unreachable from the
	// console no matter who was watching.
	confirm := func(ctx context.Context, reason, action, resource string) (bool, error) {
		if s.cfg.Prompts == nil {
			return false, errors.New("no prompt registry is wired")
		}
		ttl := s.cfg.ConfirmationTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		p, perr := s.cfg.Prompts.Create(NewPrompt{
			TurnID: turnID, Reason: reason, Channel: "console",
			TTL: ttl, Action: action, Resource: resource,
		})
		if perr != nil {
			return false, perr
		}
		emit("needs_confirmation", map[string]any{
			"prompt_id":          p.ID,
			"reason":             reason,
			"action":             action,
			"resource":           resource,
			"expires_in_seconds": int(ttl.Seconds()),
		})
		decision, werr := s.cfg.Prompts.Wait(ctx, p.ID)
		if werr != nil {
			return false, werr
		}
		return decision == PromptApproved, nil
	}

	// Deltas go down the same stream as everything else. Serialised
	// through the heartbeat's mutex-free design by writing from this
	// goroutine only — the agent calls OnDelta synchronously from its
	// read loop, so there is exactly one writer at a time here, and
	// the ticker above is the other. Guarded below.
	onDelta := func(text string) {
		if text == "" {
			return
		}
		emit("delta", map[string]any{"text": text})
	}

	resp, err := s.runBotTurn(r, claims, botID, body.Message, turnID, confirm, onDelta)
	close(done)

	if err != nil {
		// The error goes down the STREAM, not as a status: the 200 and
		// the headers are already on the wire by the time a turn can
		// fail, and a client parsing SSE has nowhere to put a status
		// code that arrives afterwards.
		emit("error", map[string]any{"message": err.Error()})
		return
	}
	// Still needing confirmation here means the ask itself failed —
	// no registry, or the wait was aborted. Say which, rather than the
	// old blanket "use another channel", which was wrong as soon as
	// this channel could ask.
	if resp.NeedsConfirmation {
		emit("error", map[string]any{
			"message": "this turn needs your approval and the prompt could not be raised: " +
				resp.ConfirmationReason,
		})
		return
	}
	emit("reply", map[string]any{
		"text":    resp.Reply,
		"turn_id": turnID,
		// Names, not a count. A count tells you a turn was busy; only
		// the names tell you whether the thing it SAYS it did is among
		// them — which is the check that catches a bot reporting work
		// it never performed.
		"tools_used":  compute.InvokedToolNames(resp.ToolCalls),
		"tool_calls":  len(resp.ToolCalls),
		"tokens_used": resp.BudgetState.Tokens,
		"cost_usd":    resp.BudgetState.SpendUSD,
		"session_id":  resp.SessionID,
	})
}

func (s *Server) runBotTurn(r *http.Request, claims *types.Claims, botID, message, turnID string,
	confirm func(context.Context, string, string, string) (bool, error),
	onDelta func(string),
) (*compute.ProcessMessageResponse, error) {
	return s.cfg.Turns.Run(r.Context(), compute.TurnRequest{
		BotID:    botID,
		Prompt:   message,
		Origin:   "console",
		OriginID: turnID,
		// The operator's claims, not the bot's: a turn somebody started
		// from the console is attributed to them, the same way a
		// scheduled routine is attributed to whoever scheduled it.
		Claims:         claims,
		TurnIDOverride: turnID,
		Channel:        botChannel,
		ChannelID:      botID,
		Confirm:        confirm,
		OnDelta:        onDelta,
	})
}

// sendSSE writes one event. Errors are dropped: the only failure here
// is a client that has gone away, and there is nowhere left to report
// that to.
func sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, body)
	flusher.Flush()
}

// BotTurnRunner is how the console starts a turn as a specific bot.
// *compute.TurnRunner satisfies it; an interface so a test can assert
// on what the handler asked for without booting a provider.
type BotTurnRunner interface {
	Run(ctx context.Context, req compute.TurnRequest) (*compute.ProcessMessageResponse, error)
}
