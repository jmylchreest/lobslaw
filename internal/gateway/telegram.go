package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// TelegramMode picks between inbound webhooks and outbound long-
// polling. Poll mode is the right default for personal deployments
// behind NAT — the bot makes outbound-only calls to the Telegram
// API, no public HTTPS endpoint required. Webhook mode is still
// supported for public cloud deployments where setWebhook is
// operationally preferable.
type TelegramMode string

const (
	TelegramModeWebhook TelegramMode = "webhook"
	TelegramModePoll    TelegramMode = "poll"
)

// TelegramConfig configures the Telegram channel — either as an
// inbound webhook receiver or an outbound long-poll client.
type TelegramConfig struct {
	// BotToken is the full Telegram Bot API token. Resolved from
	// config.toml via env:TELEGRAM_BOT_TOKEN or similar.
	BotToken string

	// Mode picks between webhook (inbound, default) and poll
	// (outbound). Empty → webhook for back-compat with Phase 6e
	// deployments.
	Mode TelegramMode

	// WebhookSecret is the random token supplied to Telegram via
	// setWebhook(secret_token=...). Every inbound update carries it
	// in the X-Telegram-Bot-Api-Secret-Token header; we reject any
	// request where it doesn't match. Required in webhook mode,
	// ignored in poll mode.
	WebhookSecret string

	// UserIDScopes maps Telegram user IDs to lobslaw security
	// scopes. Unknown users map to UnknownUserScope (or are rejected
	// when that's empty).
	UserIDScopes map[int64]string

	// UnknownUserScope is the scope assigned to unmapped user IDs.
	// Empty → reject unknown users with 403. Useful defaults:
	// "" (strict), "public" (open bot with least-privilege scope).
	UnknownUserScope string

	// DefaultBudget applies per message. Same field shape as the
	// REST channel.
	DefaultBudget compute.BudgetCaps

	// Prompts is the confirmation-prompt registry (shared with REST
	// if configured there). When nil, NeedsConfirmation surfaces
	// as plain text (Phase 6e fallback). When set, the bot sends an
	// inline keyboard with Approve / Deny buttons; the button's
	// callback_data carries the prompt ID.
	Prompts *PromptRegistry

	// ConfirmationTTL mirrors RESTConfig.ConfirmationTTL. 0 → 5min.
	ConfirmationTTL time.Duration

	// HTTPClient is the client used to POST replies back to the
	// Telegram Bot API. Nil → a new http.Client with 30s timeout.
	// Injectable for tests that want to intercept the reply path.
	HTTPClient *http.Client

	// APIBase is the Telegram Bot API URL. Default
	// "https://api.telegram.org". Overridable for tests that use
	// an httptest.Server.
	APIBase string

	// Logger is used for structured log output. Nil → slog.Default().
	Logger *slog.Logger
}

// TelegramHandler is the webhook receiver. Mounted on the REST
// server's mux at /telegram so HTTPS + port are shared. Stateless
// per request except for the HTTP client (connection pool).
type TelegramHandler struct {
	cfg    TelegramConfig
	agent  *compute.Agent
	log    *slog.Logger
	client *http.Client
	base   string

	// inflightMu guards the de-dup cache. Telegram retries on
	// network errors; without dedup a tool invocation could run
	// twice for one user intent.
	inflightMu sync.Mutex
	seenUpdate map[int64]time.Time // update_id → first-seen time

	// continuations hold the agent-turn state needed to resume a
	// turn after the user taps Approve on a confirmation keyboard.
	// Keyed by prompt ID (same ID that appears in callback_data).
	// Populated by sendConfirmationKeyboard; drained by
	// handleCallbackQuery. Entries aren't persisted — a restart
	// loses in-flight continuations (the user's tap becomes a no-op
	// with a "no longer exists" reply from the registry).
	continuationsMu sync.Mutex
	continuations   map[string]*telegramContinuation
}

// telegramContinuation captures everything handleCallbackQuery
// needs to resume an in-flight turn on approval.
type telegramContinuation struct {
	req      compute.ProcessMessageRequest
	messages []compute.Message
	chatID   int64
	reason   string
}

// Telegram Update / Message types — minimal subset we consume. The
// upstream API is huge; only model what the handler actually reads.

type tgUpdate struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message,omitempty"`
	// CallbackQuery is populated for inline-keyboard taps (Phase 6f).
	CallbackQuery *tgCallbackQuery `json:"callback_query,omitempty"`
}

type tgMessage struct {
	MessageID int64   `json:"message_id"`
	From      *tgUser `json:"from,omitempty"`
	Chat      tgChat  `json:"chat"`
	Text      string  `json:"text,omitempty"`
	Date      int64   `json:"date"`
}

type tgUser struct {
	ID        int64  `json:"id"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from,omitempty"`
	Message *tgMessage `json:"message,omitempty"`
	Data    string     `json:"data,omitempty"`
}

// NewTelegramHandler constructs a handler with injected dependencies.
// Fails at construction when BotToken or WebhookSecret is missing —
// neither is optional, and a misconfigured handler would either
// accept anyone's traffic or fail silently to reply.
func NewTelegramHandler(cfg TelegramConfig, agent *compute.Agent) (*TelegramHandler, error) {
	if cfg.BotToken == "" {
		return nil, errors.New("telegram: BotToken required")
	}
	if cfg.Mode == "" {
		cfg.Mode = TelegramModeWebhook
	}
	if cfg.Mode == TelegramModeWebhook && cfg.WebhookSecret == "" {
		return nil, errors.New("telegram: WebhookSecret required in webhook mode (use setWebhook secret_token) — or set mode=poll")
	}
	if cfg.Mode != TelegramModeWebhook && cfg.Mode != TelegramModePoll {
		return nil, fmt.Errorf("telegram: unknown mode %q; want %q or %q",
			cfg.Mode, TelegramModeWebhook, TelegramModePoll)
	}
	if agent == nil {
		return nil, errors.New("telegram: agent required")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base := cfg.APIBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &TelegramHandler{
		cfg:           cfg,
		agent:         agent,
		log:           logger,
		client:        client,
		base:          base,
		seenUpdate:    make(map[int64]time.Time),
		continuations: make(map[string]*telegramContinuation),
	}, nil
}

// ServeHTTP is the webhook receiver. Methods other than POST get
// 405; missing or wrong secret-token header gets 401; unknown
// user (with UnknownUserScope empty) gets 403; bad JSON or unknown
// update shape gets 200 + empty ack so Telegram doesn't retry
// forever on a misformatted update (we log the oddity server-side
// instead).
func (h *TelegramHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Secret-token header — the webhook-auth mechanism. Empty header
	// or mismatch = reject with 401. We do a constant-time compare
	// against the configured secret to resist timing attacks.
	got := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
	if !constantTimeEq(got, h.cfg.WebhookSecret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var up tgUpdate
	if err := json.NewDecoder(r.Body).Decode(&up); err != nil {
		h.log.Warn("telegram: malformed update body",
			"err", err, "remote", r.RemoteAddr)
		// 200 OK on bad JSON — Telegram re-queues on non-2xx, and
		// a malformed update isn't going to un-malform on retry.
		w.WriteHeader(http.StatusOK)
		return
	}
	h.dispatchUpdate(r.Context(), &up)
	w.WriteHeader(http.StatusOK)
}

// handleMessage dispatches a text message to the agent and posts
// the reply back via sendMessage. Runs synchronously within the
// webhook handler so Telegram sees "OK" only after the reply has
// been sent — gives the bot API visibility into our reply latency
// via its internal metrics.
func (h *TelegramHandler) handleMessage(ctx context.Context, msg *tgMessage) {
	scope, ok := h.resolveScope(msg.From)
	if !ok {
		h.log.Warn("telegram: unknown user, UnknownUserScope empty — dropping",
			"user_id", userIDOf(msg.From),
			"username", usernameOf(msg.From))
		// No reply — silent drop for unmapped users. Operators who
		// want a rejection message wire a narrow-scope bot.
		return
	}

	budget, err := compute.NewTurnBudget(h.cfg.DefaultBudget)
	if err != nil {
		h.log.Error("telegram: budget init failed", "err", err)
		return
	}

	claims := &types.Claims{
		UserID: tgUserIdentity(msg.From),
		Scope:  scope,
	}
	turnID := "tg-" + strconv.FormatInt(msg.MessageID, 10)
	agentReq := compute.ProcessMessageRequest{
		Message: msg.Text,
		Claims:  claims,
		TurnID:  turnID,
		Budget:  budget,
	}

	resp, err := h.agent.RunToolCallLoop(ctx, agentReq)
	if err != nil {
		h.log.Error("telegram: agent error",
			"turn_id", turnID, "err", err)
		h.sendText(msg.Chat.ID, "Something went wrong processing your message.")
		return
	}

	switch {
	case resp.NeedsConfirmation:
		if h.cfg.Prompts != nil {
			h.sendConfirmationKeyboard(msg.Chat.ID, agentReq, resp)
			return
		}
		// Fallback: no registry wired — render the reason as plain text.
		h.sendText(msg.Chat.ID, "Confirmation required: "+resp.ConfirmationReason)
	case resp.Reply == "":
		h.sendText(msg.Chat.ID, "(empty reply)")
	default:
		h.sendText(msg.Chat.ID, resp.Reply)
	}
}

// sendConfirmationKeyboard registers a prompt in the shared
// registry and sends a sendMessage with an inline keyboard whose
// buttons carry the prompt ID as callback_data. The user's tap
// fires a callback_query update; handleCallbackQuery resolves the
// registry entry accordingly.
//
// The reply_markup shape matches Telegram's InlineKeyboardMarkup:
// {"inline_keyboard": [[{text, callback_data}, ...]]}. Callback
// data is prefixed "prompt:approve:<id>" / "prompt:deny:<id>" so
// the handler can parse the verb + id without a separate mapping.
func (h *TelegramHandler) sendConfirmationKeyboard(chatID int64, req compute.ProcessMessageRequest, resp *compute.ProcessMessageResponse) {
	ttl := h.cfg.ConfirmationTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	p, err := h.cfg.Prompts.Create(req.TurnID, resp.ConfirmationReason, "telegram", ttl)
	if err != nil {
		h.log.Error("telegram: prompt registration failed", "err", err)
		h.sendText(chatID, "Confirmation required: "+resp.ConfirmationReason)
		return
	}

	// Stash resume state so handleCallbackQuery can re-enter the
	// agent loop on approval. Dropped if the user denies or the
	// prompt times out — nothing periodically reaps entries because
	// every code path that reads this map also deletes on the way
	// out (approve, deny, or missing on re-tap).
	h.continuationsMu.Lock()
	h.continuations[p.ID] = &telegramContinuation{
		req:      req,
		messages: resp.Messages,
		chatID:   chatID,
		reason:   resp.ConfirmationReason,
	}
	h.continuationsMu.Unlock()

	body := map[string]any{
		"chat_id": chatID,
		"text":    "Confirmation required: " + resp.ConfirmationReason,
		"reply_markup": map[string]any{
			"inline_keyboard": [][]map[string]string{
				{
					{"text": "Approve", "callback_data": "prompt:approve:" + p.ID},
					{"text": "Deny", "callback_data": "prompt:deny:" + p.ID},
				},
			},
		},
	}
	h.postJSON("sendMessage", body)
}

// takeContinuation pops and returns the stored continuation for the
// given prompt ID. Returns (nil, false) when no entry exists — the
// prompt may have been resolved on a different channel, reaped, or
// never existed. Callers surface a "no longer exists" message in
// that case.
func (h *TelegramHandler) takeContinuation(promptID string) (*telegramContinuation, bool) {
	h.continuationsMu.Lock()
	defer h.continuationsMu.Unlock()
	c, ok := h.continuations[promptID]
	if ok {
		delete(h.continuations, promptID)
	}
	return c, ok
}

// handleCallbackQuery resolves a pending prompt based on the
// callback_data tag format "prompt:<verb>:<id>" produced by
// sendConfirmationKeyboard. Any other callback_data shape is
// logged + ignored (forward-compatible with future button types).
//
// The tap is acknowledged with answerCallbackQuery so Telegram
// removes the "loading" spinner on the user's side; the resolution
// outcome is surfaced via a plain sendMessage confirmation.
func (h *TelegramHandler) handleCallbackQuery(ctx context.Context, q *tgCallbackQuery) {
	// Always ack the callback so the client UI stops spinning.
	defer h.postJSON("answerCallbackQuery", map[string]any{
		"callback_query_id": q.ID,
	})

	parts := strings.SplitN(q.Data, ":", 3)
	if len(parts) != 3 || parts[0] != "prompt" {
		h.log.Debug("telegram: unhandled callback_data shape", "data", q.Data)
		return
	}
	verb, promptID := parts[1], parts[2]

	var decision PromptDecision
	var reply string
	switch verb {
	case "approve":
		decision = PromptApproved
		reply = "Approved."
	case "deny":
		decision = PromptDenied
		reply = "Denied."
	default:
		h.log.Debug("telegram: unknown prompt verb", "verb", verb, "data", q.Data)
		return
	}

	if h.cfg.Prompts == nil {
		h.log.Warn("telegram: callback arrived but no prompt registry configured")
		return
	}
	if err := h.cfg.Prompts.Resolve(promptID, decision); err != nil {
		switch {
		case errors.Is(err, ErrPromptNotFound):
			reply = "That prompt no longer exists."
		case errors.Is(err, ErrPromptResolved):
			reply = "That prompt was already resolved."
		default:
			h.log.Error("telegram: resolve failed", "err", err, "id", promptID)
			reply = "Couldn't process the response."
		}
		// Resolution failed or was redundant — drop any stored
		// continuation to avoid leaking memory on repeat taps.
		_, _ = h.takeContinuation(promptID)
		if q.Message != nil {
			h.sendText(q.Message.Chat.ID, reply)
		}
		return
	}

	if decision == PromptDenied {
		_, _ = h.takeContinuation(promptID) // drop state, nothing to resume
		if q.Message != nil {
			h.sendText(q.Message.Chat.ID, reply)
		}
		return
	}

	// Approved — acknowledge + resume the turn.
	if q.Message != nil {
		h.sendText(q.Message.Chat.ID, reply)
	}
	cont, ok := h.takeContinuation(promptID)
	if !ok {
		// Approval with no stored state — probably a bot restart
		// after the keyboard was sent. Nothing to resume.
		h.log.Warn("telegram: approve with no continuation state", "prompt_id", promptID)
		if q.Message != nil {
			h.sendText(q.Message.Chat.ID, "I've lost track of that turn — send it again.")
		}
		return
	}
	h.resumeAfterApproval(ctx, cont)
}

// resumeAfterApproval re-enters the agent loop with a relaxed
// budget and sends the final reply (or a new keyboard if another
// confirmation is needed) back to the originating chat. Kept as a
// method so callers can also invoke it from tests.
func (h *TelegramHandler) resumeAfterApproval(ctx context.Context, cont *telegramContinuation) {
	cont.req.Budget.Relax()
	resp, err := h.agent.ResumeFromConfirmation(ctx, cont.req, cont.messages)
	if err != nil {
		h.log.Error("telegram: resume failed",
			"turn_id", cont.req.TurnID, "err", err)
		h.sendText(cont.chatID, "Something went wrong continuing your request.")
		return
	}
	switch {
	case resp.NeedsConfirmation:
		h.sendConfirmationKeyboard(cont.chatID, cont.req, resp)
	case resp.Reply == "":
		h.sendText(cont.chatID, "(empty reply)")
	default:
		h.sendText(cont.chatID, resp.Reply)
	}
}

// postJSON POSTs to a bot API method with a JSON body. Shared by
// sendText and the inline-keyboard paths.
func (h *TelegramHandler) postJSON(method string, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		h.log.Error("telegram: marshal "+method, "err", err)
		return
	}
	url := fmt.Sprintf("%s/bot%s/%s", h.base, h.cfg.BotToken, method)
	resp, err := h.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		h.log.Error("telegram: POST "+method+" failed", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		h.log.Error("telegram: "+method+" non-2xx",
			"status", resp.StatusCode, "body", string(raw))
	}
}

// resolveScope maps a Telegram user → lobslaw scope. Returns
// (scope, true) when resolved, (_, false) when the user is unknown
// AND no default is configured.
func (h *TelegramHandler) resolveScope(from *tgUser) (string, bool) {
	if from == nil {
		return h.cfg.UnknownUserScope, h.cfg.UnknownUserScope != ""
	}
	if scope, ok := h.cfg.UserIDScopes[from.ID]; ok {
		return scope, true
	}
	if h.cfg.UnknownUserScope != "" {
		return h.cfg.UnknownUserScope, true
	}
	return "", false
}

// sendText POSTs to the Bot API's sendMessage endpoint. Errors are
// logged but don't propagate — there's nothing useful to do with a
// failed send at this layer. Telegram will deliver eventually if
// it's a transient network issue.
func (h *TelegramHandler) sendText(chatID int64, text string) {
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		h.log.Error("telegram: marshal sendMessage body", "err", err)
		return
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", h.base, h.cfg.BotToken)
	resp, err := h.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		h.log.Error("telegram: POST sendMessage failed", "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		h.log.Error("telegram: sendMessage non-2xx",
			"status", resp.StatusCode, "body", string(raw))
	}
}

// firstSeen returns true if update_id is new. Entries older than
// 5 minutes are reaped during the check so the map stays bounded.
func (h *TelegramHandler) firstSeen(updateID int64) bool {
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()

	// Opportunistic reap. O(n) across every call which is fine for
	// personal-scale bots (tens of updates/minute at most); upgrade
	// to a proper LRU if a deployment ever hits tens of thousands.
	now := time.Now()
	for id, t := range h.seenUpdate {
		if now.Sub(t) > 5*time.Minute {
			delete(h.seenUpdate, id)
		}
	}

	if _, seen := h.seenUpdate[updateID]; seen {
		return false
	}
	h.seenUpdate[updateID] = now
	return true
}

// constantTimeEq is a timing-attack-resistant string compare.
// Avoids subtle.ConstantTimeCompare's requirement that both slices
// be equal length (we want a mismatch-on-length to also be
// constant time w.r.t. the matching prefix).
func constantTimeEq(a, b string) bool {
	if len(a) != len(b) {
		// Length mismatch: we can't XOR byte pairs (b is a different
		// length — indexing b[i] here would panic or, worse, compare
		// against the wrong bytes). Still touch every byte of a so
		// this branch takes time proportional to len(a), matching
		// the equal-length branch's work shape. The write to acc
		// (never compared later) is only there to prevent a future
		// compiler from deciding the whole loop is dead code.
		var acc byte
		for i := 0; i < len(a); i++ {
			acc |= a[i]
		}
		_ = acc
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// Helpers for nil-safe user extraction.
func tgUserIdentity(u *tgUser) string {
	if u == nil {
		return "tg-unknown"
	}
	if u.Username != "" {
		return "tg-@" + u.Username
	}
	return "tg-" + strconv.FormatInt(u.ID, 10)
}

func userIDOf(u *tgUser) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

func usernameOf(u *tgUser) string {
	if u == nil {
		return ""
	}
	return strings.TrimSpace(u.Username)
}

// Mode returns the handler's active mode. Used by the gateway to
// decide whether to mount the webhook route or start the poll loop.
func (h *TelegramHandler) Mode() TelegramMode { return h.cfg.Mode }

// dispatchUpdate is the post-decode path shared by the webhook
// ServeHTTP and the poll loop. Dedup + update-shape dispatch live
// here so both transports behave identically.
func (h *TelegramHandler) dispatchUpdate(ctx context.Context, up *tgUpdate) {
	if !h.firstSeen(up.UpdateID) {
		h.log.Info("telegram: duplicate update ignored", "update_id", up.UpdateID)
		return
	}
	switch {
	case up.Message != nil && up.Message.Text != "":
		h.handleMessage(ctx, up.Message)
	case up.CallbackQuery != nil:
		h.handleCallbackQuery(ctx, up.CallbackQuery)
	default:
		h.log.Debug("telegram: unsupported update shape", "update_id", up.UpdateID)
	}
}

// pollDefaults tune the long-poll loop. Chosen to balance Telegram
// API etiquette (long-poll timeout 25s = a quarter of their 60s
// server-side max) against backoff behaviour on flaky networks.
const (
	pollLongTimeout   = 25 * time.Second
	pollInitialBackoff = 1 * time.Second
	pollMaxBackoff     = 30 * time.Second
	pollBackoffFactor  = 1.8
)

// tgGetUpdatesResp is the response shape for the Bot API getUpdates
// endpoint: {ok, result, description}.
type tgGetUpdatesResp struct {
	OK          bool       `json:"ok"`
	Result      []tgUpdate `json:"result"`
	Description string     `json:"description,omitempty"`
	ErrorCode   int        `json:"error_code,omitempty"`
}

// RunLongPoll blocks on the getUpdates loop until ctx is cancelled.
// Only valid in poll mode; returns an error immediately otherwise.
//
// Algorithm mirrors openclaw/openclaw's polling session:
//   1. loop getUpdates(offset=next, timeout=25s) — Telegram holds
//      the connection until updates arrive or the timeout expires
//   2. for each update: advance offset, dispatch through the same
//      path the webhook uses (dispatchUpdate)
//   3. on transport error: exponential backoff 1s→30s (factor 1.8)
//   4. on HTTP 409 Conflict: a webhook is registered — call
//      deleteWebhook, then resume. Telegram refuses getUpdates while
//      a webhook is live, so this recovers the stuck state.
//
// Offset is kept in-memory only. A restart re-calls getUpdates with
// offset=0, which returns everything Telegram has buffered
// (< 24h retention). Duplicates are caught by the shared firstSeen
// cache downstream.
func (h *TelegramHandler) RunLongPoll(ctx context.Context) error {
	if h.cfg.Mode != TelegramModePoll {
		return fmt.Errorf("telegram: RunLongPoll called with mode=%q", h.cfg.Mode)
	}
	h.log.Info("telegram: long-poll loop starting")

	var (
		nextOffset int64
		backoff    = pollInitialBackoff
	)
	for {
		if ctx.Err() != nil {
			h.log.Info("telegram: long-poll loop exiting", "reason", ctx.Err())
			return nil
		}

		updates, newOffset, err := h.getUpdates(ctx, nextOffset, pollLongTimeout)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			if isWebhookConflict(err) {
				h.log.Warn("telegram: getUpdates 409 (webhook still registered); deleting webhook")
				if delErr := h.deleteWebhook(ctx); delErr != nil {
					h.log.Warn("telegram: deleteWebhook failed", "err", delErr)
				}
				// Don't sleep — jump straight back to getUpdates.
				continue
			}
			h.log.Warn("telegram: getUpdates error; backing off",
				"err", err, "backoff", backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff)
			continue
		}
		backoff = pollInitialBackoff

		for i := range updates {
			h.dispatchUpdate(ctx, &updates[i])
		}
		if newOffset > nextOffset {
			nextOffset = newOffset
		}
	}
}

// getUpdates calls the Bot API's getUpdates with the supplied offset
// and long-poll timeout. Returns the decoded updates and the offset
// to pass on the next call (lastUpdateID + 1).
func (h *TelegramHandler) getUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]tgUpdate, int64, error) {
	body := map[string]any{
		"timeout": int(timeout.Seconds()),
	}
	if offset > 0 {
		body["offset"] = offset
	}
	buf, _ := json.Marshal(body)

	// The HTTP client's own timeout must exceed the long-poll
	// timeout — otherwise we cancel the request before Telegram
	// gets a chance to reply.
	reqCtx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()

	url := fmt.Sprintf("%s/bot%s/getUpdates", h.base, h.cfg.BotToken)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusConflict {
		return nil, 0, errWebhookConflict
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("getUpdates: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var decoded tgGetUpdatesResp
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, 0, fmt.Errorf("getUpdates: decode: %w", err)
	}
	if !decoded.OK {
		return nil, 0, fmt.Errorf("getUpdates: telegram said ok=false: %s", decoded.Description)
	}
	var maxID int64
	for _, u := range decoded.Result {
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}
	}
	var next int64
	if maxID > 0 {
		next = maxID + 1
	}
	return decoded.Result, next, nil
}

// errWebhookConflict signals a 409 from getUpdates — Telegram
// refuses getUpdates while a webhook is registered. Handled inside
// RunLongPoll by calling deleteWebhook.
var errWebhookConflict = errors.New("telegram: getUpdates returned 409 (webhook conflict)")

func isWebhookConflict(err error) bool { return errors.Is(err, errWebhookConflict) }

// deleteWebhook clears any registered webhook so getUpdates works.
// No-op if no webhook is set.
func (h *TelegramHandler) deleteWebhook(ctx context.Context) error {
	url := fmt.Sprintf("%s/bot%s/deleteWebhook", h.base, h.cfg.BotToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("deleteWebhook: HTTP %d", resp.StatusCode)
	}
	return nil
}

func nextBackoff(current time.Duration) time.Duration {
	next := time.Duration(float64(current) * pollBackoffFactor)
	if next > pollMaxBackoff {
		return pollMaxBackoff
	}
	return next
}
