package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

// handleBotChat serves POST /v1/bots/{id}/messages.
//
// Talking to a SPECIFIC bot, which /v1/messages cannot do — that
// endpoint is the coordinator's, shared with Telegram and Slack, and a
// console that could only reach the coordinator would make every
// specialist something you can configure but not converse with.
//
// Streamed as Server-Sent Events. A bot turn can run tools for a
// minute, and a request that returns nothing until it finishes is
// indistinguishable from one that has hung — which is how somebody
// ends up reloading and starting a second turn.
//
// The turn goes through turn.Runner, never a concrete agent: the
// gateway is the transport and must not depend on internal/compute.
const botChatHeartbeat time.Duration = 10 * time.Second

func (s *Server) handleBotChat(w http.ResponseWriter, r *http.Request, botID string) {
	// Authenticate before the 200 and the SSE headers go out, so an
	// unauthenticated caller gets a 401 rather than a streamed error
	// over a successful status line. Cookie-aware, so the web console's
	// login session reaches this route.
	authn, authErr := s.authenticateRequest(r)
	if authErr != nil {
		s.jsonErr(w, http.StatusUnauthorized, authErr.Error())
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.runner == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node cannot run turns")
		return
	}
	if r.Method != http.MethodPost {
		s.jsonErr(w, http.StatusMethodNotAllowed, "POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

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
	turnID := ids.New()
	claims := authn.Claims
	userID := ""
	if claims != nil {
		userID = claims.UserID
	}
	sessionRef := SessionRef{Channel: botChannel, ChannelID: botID, UserID: userID}
	ctx, stopStream := s.bindStream(r.Context(), authn.LoginID)
	defer stopStream()
	lease, disposition := s.gate.Acquire(ctx, cacheKey(sessionRef), turnID, body.Message)
	if disposition == Folded {
		s.jsonErr(w, http.StatusAccepted, "message folded into an in-flight turn; its reply covers this message")
		return
	}
	if disposition == Dropped {
		s.jsonErr(w, http.StatusConflict, "a turn is already running for this bot")
		return
	}
	defer lease.Release()
	if len(lease.Batch) > 1 {
		body.Message = strings.Join(lease.Batch, "\n")
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Without flushing, SSE is just a slow JSON response that
		// arrives all at once — worse than admitting it.
		s.jsonErr(w, http.StatusInternalServerError, "this server cannot stream")
		return
	}
	// Clear the write deadline for THIS response only. The server's
	// WriteTimeout is an absolute deadline from the start of the
	// response, so a bot turn against a real model routinely runs past
	// it and every one would be killed mid-stream even though the turn
	// completed and was recorded. Scoped here so a stuck request
	// anywhere else still hits the server-wide bound.
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

	// Three goroutines can write to this stream: the heartbeat ticker,
	// and this handler. An http.ResponseWriter is not safe for
	// concurrent use, so one guarded emitter rather than a mutex the
	// call sites take themselves.
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
	exited := make(chan struct{})
	defer func() { close(done); <-exited }()
	go func() {
		defer close(exited)
		ticker := time.NewTicker(botChatHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				emit("working", map[string]any{"turn_id": turnID})
			}
		}
	}()

	// The bot's conversations live on their own synthetic channel, so
	// its working transcripts never appear in somebody's chat history.
	prior := s.conv.Load(r.Context(), sessionRef)

	req := turn.Request{
		Message: body.Message,
		Claims:  claims,
		// The bot's principal, so its memory belongs to it; the claims
		// stay the operator's, so policy still answers to the person
		// who asked.
		Principal:           identity.Bot(botID),
		BotID:               botID,
		TurnID:              turnID,
		Channel:             botChannel,
		ChannelID:           botID,
		Caps:                s.cfg.DefaultBudget,
		ConversationHistory: prior.Messages,
		ConversationSummary: prior.Summary,
	}

	resp, err := s.runner.Run(ctx, req)
	if err != nil {
		// The error goes down the STREAM, not as a status: the 200 and
		// the headers are already on the wire.
		emit("error", map[string]any{"message": err.Error()})
		return
	}
	turnStart := resp.TurnStartIndex

	// Confirmation loop, as on /v1/messages: raise a prompt, push its
	// id down the wire for the console to render buttons against, and
	// block until the person on the other end of the stream answers or
	// the TTL fires.
	for resp.NeedsConfirmation && s.cfg.Prompts != nil {
		ttl := s.cfg.ConfirmationTTL
		if ttl <= 0 {
			ttl = 5 * time.Minute
		}
		p, perr := s.cfg.Prompts.Create(NewPrompt{
			TurnID:    turnID,
			Reason:    resp.ConfirmationReason,
			Channel:   botChannel,
			SessionID: botID,
			TTL:       ttl,
			Action:    resp.ConfirmationAction,
			Resource:  resp.ConfirmationResource,
			RaisedFor: userID,
		})
		if perr != nil {
			emit("error", map[string]any{
				"message": "this turn needs your approval and the prompt could not be raised: " +
					resp.ConfirmationReason,
			})
			return
		}
		emit("needs_confirmation", map[string]any{
			"prompt_id":          p.ID,
			"reason":             resp.ConfirmationReason,
			"action":             resp.ConfirmationAction,
			"resource":           resp.ConfirmationResource,
			"expires_in_seconds": int(ttl.Seconds()),
		})

		decision, werr := s.cfg.Prompts.Wait(ctx, p.ID)
		if werr != nil {
			emit("error", map[string]any{
				"message": "the approval request was aborted: " + werr.Error(),
			})
			return
		}
		if decision != PromptApproved {
			emit("reply", map[string]any{
				"text": fmt.Sprintf("Confirmation %s: %s", decision.String(), resp.ConfirmationReason),
			})
			return
		}

		req.Spent = resp.BudgetState
		resumeCtx := turn.WithTurnApproval(ctx, resp.ConfirmationAction, resp.ConfirmationResource)
		resumed, rerr := s.runner.Resume(resumeCtx, req, resp.Messages)
		if rerr != nil {
			emit("error", map[string]any{"message": rerr.Error()})
			return
		}
		resumed.ToolCalls = append(resp.ToolCalls, resumed.ToolCalls...)
		resp = resumed
	}

	// Persisted once, complete, after the confirmation loop so an
	// approved-and-resumed turn is not recorded twice in halves.
	if newTurn := newTurnMessages(resp.Messages, turnStart); len(newTurn) > 0 {
		s.conv.Append(r.Context(), sessionRef, turnID, newTurn)
	}

	// Still needing confirmation here means the ask itself could not be
	// raised — no prompt registry, or the loop was skipped. Say so
	// rather than sending an empty reply the console would render as a
	// silent turn.
	if resp.NeedsConfirmation {
		emit("error", map[string]any{
			"message": "this turn needs your approval and no prompt registry is wired: " +
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
		"tools_used":  invokedToolNames(resp.ToolCalls),
		"tool_calls":  len(resp.ToolCalls),
		"tokens_used": 0,
		"cost_usd":    resp.BudgetState.SpendUSD,
		"session_id":  botChannel + ":" + botID,
	})
}

// invokedToolNames is the distinct tools a turn actually invoked,
// sorted for a stable rendering. The result text is the bot's account
// of its work; this is the record of it.
func invokedToolNames(calls []turn.ToolInvocation) []string {
	seen := make(map[string]struct{}, len(calls))
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		if c.ToolName == "" {
			continue
		}
		if _, ok := seen[c.ToolName]; ok {
			continue
		}
		seen[c.ToolName] = struct{}{}
		out = append(out, c.ToolName)
	}
	sort.Strings(out)
	return out
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
