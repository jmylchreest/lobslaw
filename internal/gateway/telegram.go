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
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/singleton"
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

	// TypingInterval refreshes the typing indicator (Telegram
	// clears it at ~5s). 0 disables. Default 4s applied by
	// handleMessage when unset.
	TypingInterval time.Duration

	// InterimTimeout sends a "still working on this" message if
	// the turn exceeds this duration AND the SOUL's directness
	// score is low (chatty). 0 disables. Default 30s.
	InterimTimeout time.Duration

	// HardTimeout cancels the turn context and lets the agent's
	// forceSummaryReply produce a graceful user-visible wrap-up.
	// 0 disables. Default 90s.
	HardTimeout time.Duration

	// Soul supplies the current SoulConfig on demand. Used to gate
	// interim messages on EmotiveStyle.Directness — high directness
	// (>=7) means "no filler chatter", so interim messages are
	// suppressed. Nil → interim messages are universal.
	Soul func() *types.SoulConfig

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

	// Gate, when non-nil, restricts the long-poll loop to nodes that
	// own the "telegram-poll" singleton — typically the raft leader.
	// Nil → poll unconditionally (single-node / gateway-only setups).
	Gate singleton.Gate

	// ChannelState persists the Telegram update_id offset across
	// restarts. Without it every restart calls getUpdates(offset=0),
	// Telegram replays its 24h backlog, the agent re-processes every
	// recent message including request-completion replies, and the
	// user gets duplicate commitments + duplicate replies. Nil → no
	// persistence (the legacy in-memory-only behaviour, fine for
	// tests + ephemeral single-shot runs).
	ChannelState ChannelStateStore
}

// ChannelStateStore is a minimal raft-backed key-value interface for
// gateway channels to persist resume state (Telegram update offset,
// REST cursors, webhook timestamps). Implemented by the memory
// package; gateway just consumes the contract.
type ChannelStateStore interface {
	Get(ctx context.Context, channel, key string) ([]byte, error)
	Put(ctx context.Context, channel, key string, value []byte) error
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

	// history is the per-chat rolling conversation buffer feeding
	// ProcessMessageRequest.ConversationHistory. Stateless across
	// restarts — persistent recall is the episodic-memory tool's
	// responsibility, not this cache.
	history *chatHistory
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
	Caption   string  `json:"caption,omitempty"`
	Date      int64   `json:"date"`

	// Media — presence (non-nil/non-empty) means the user sent
	// something we can't process as text yet. We acknowledge with a
	// friendly reply rather than silently dropping. Full vision /
	// audio handling (download + pass to a multi-modal model) is
	// deferred — see DEFERRED.md → "telegram media handling".
	Photo    []tgPhotoSize `json:"photo,omitempty"`
	Voice    *tgFileMeta   `json:"voice,omitempty"`
	Audio    *tgFileMeta   `json:"audio,omitempty"`
	Video    *tgFileMeta   `json:"video,omitempty"`
	Document *tgFileMeta   `json:"document,omitempty"`
	Sticker  *tgFileMeta   `json:"sticker,omitempty"`
}

// tgPhotoSize is one rendition of a photo Telegram delivered. The
// API returns multiple sizes; the largest is conventionally used.
type tgPhotoSize struct {
	FileID   string `json:"file_id"`
	FileSize int    `json:"file_size,omitempty"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
}

// tgFileMeta is the minimal shape we read for non-photo media. We
// don't download anything from this today — the dispatcher just
// detects the field's presence to give a useful reply.
type tgFileMeta struct {
	FileID   string `json:"file_id"`
	MimeType string `json:"mime_type,omitempty"`
	FileSize int    `json:"file_size,omitempty"`
	Duration int    `json:"duration,omitempty"`
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
		// Telegram poll + send traffic routes through egress under
		// role "gateway/telegram" — ACL is hardcoded to
		// api.telegram.org so a compromised process can't redirect
		// our bot's traffic to an attacker-controlled host.
		base := egress.For("gateway/telegram").HTTPClient()
		wrapped := *base
		wrapped.Timeout = 30 * time.Second
		client = &wrapped
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
		history:       newChatHistory(0, 0),
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

	h.log.Debug("telegram: message received",
		"turn_id", turnID,
		"user_id", userIDOf(msg.From),
		"username", usernameOf(msg.From),
		"scope", scope,
		"text_len", len(msg.Text))

	priorHistory := h.history.Load(msg.Chat.ID)
	h.log.Debug("telegram: conversation history loaded",
		"turn_id", turnID,
		"chat_id", msg.Chat.ID,
		"prior_turns", len(priorHistory))

	// Convert wire format → channel-agnostic IncomingMessage and
	// download any attachments to /workspace/incoming/<turn>/ so
	// the agent's MCP tools (e.g. minimax.image_understanding) can
	// open them by path. Best-effort: a download failure on one
	// attachment doesn't fail the turn.
	im := telegramMessageToIncoming(msg)
	if err := h.downloadAttachments(ctx, turnID, &im); err != nil {
		h.log.Warn("telegram: attachment download dir prep failed", "err", err, "turn_id", turnID)
	}

	// When the user sent media-only (no text), use the caption as
	// the message body so the agent has something to anchor on.
	// Falls back to a stub the agent can interpret as "user sent
	// just media; do something useful with it".
	turnText := msg.Text
	if turnText == "" {
		turnText = msg.Caption
	}
	if turnText == "" && im.HasMedia() {
		turnText = "(no caption — please inspect the attached media and respond)"
	}

	agentReq := compute.ProcessMessageRequest{
		Message:             turnText,
		Attachments:         im.Attachments,
		Claims:              claims,
		TurnID:              turnID,
		Budget:              budget,
		ConversationHistory: priorHistory,
		Channel:             "telegram",
		ChannelID:           strconv.FormatInt(msg.Chat.ID, 10),
	}

	// Wrap the agent call with the responsiveness guards: typing
	// indicator keep-alive, interim "still working" message (if
	// SOUL personality allows), and a hard-timeout context that
	// triggers forceSummaryReply inside the agent rather than
	// silent failure.
	turnCtx, cleanup := h.startResponsivenessGuards(ctx, msg.Chat.ID)
	defer cleanup()

	resp, err := h.agent.RunToolCallLoop(turnCtx, agentReq)
	if err != nil {
		h.log.Error("telegram: agent error",
			"turn_id", turnID, "err", err)
		h.sendText(msg.Chat.ID, "Something went wrong processing your message.")
		return
	}

	// Surface policy denials to the user directly — these are
	// otherwise opaque (LLM may or may not narrate them). Emit one
	// interstitial per denial so the user sees exactly what was
	// blocked and why.
	h.notifyPolicyDenials(msg.Chat.ID, resp.ToolCalls)

	// Persist the FULL message thread from this turn (user message,
	// every intermediate assistant with tool_calls, every tool
	// result, and the final assistant reply). Without this the bot
	// has no record of its own actions on follow-up turns — leading
	// to the "I don't have a web fetch tool" lie when the user asks
	// "why did you do X" after a turn that DID do X. Cap is on
	// total messages (not turns) so a single multi-tool turn
	// doesn't permanently swamp the buffer; oldest messages drop
	// when the cap is hit.
	if newTurn := newTurnMessages(resp.Messages, len(priorHistory)); len(newTurn) > 0 {
		h.history.Append(msg.Chat.ID, newTurn...)
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
// Send is the public proactive-message entry point. Identical to
// sendText except errors propagate to the caller instead of being
// logged and swallowed. Used by the compute-layer notify_telegram
// builtin so scheduled tasks can deliver replies to chats they
// weren't invoked from. Safe to call concurrently — the underlying
// http.Client is a pool.
func (h *TelegramHandler) Send(chatID int64, text string) error {
	body := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram: marshal sendMessage: %w", err)
	}
	url := fmt.Sprintf("%s/bot%s/sendMessage", h.base, h.cfg.BotToken)
	resp, err := h.client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("telegram: POST sendMessage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("telegram: sendMessage non-2xx (HTTP %d): %s", resp.StatusCode, string(raw))
	}
	return nil
}

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
	case up.Message != nil && (up.Message.Text != "" || messageHasMedia(up.Message)):
		// Text, caption-with-media, and media-only all route to the
		// agent. The agent sees attachment metadata + LocalPath
		// (after download) and decides whether to reply directly or
		// call an MCP tool (e.g. minimax.image_understanding) to
		// inspect the media.
		h.handleMessage(ctx, up.Message)
	case up.CallbackQuery != nil:
		h.handleCallbackQuery(ctx, up.CallbackQuery)
	default:
		h.log.Debug("telegram: unsupported update shape", "update_id", up.UpdateID)
	}
}

// messageHasMedia reports whether the inbound carries any of the
// attachment fields lobslaw recognises. Caption-without-media is
// treated as text via the empty Text branch above; this only fires
// when Text is empty AND a media field is present.
func messageHasMedia(m *tgMessage) bool {
	if m == nil {
		return false
	}
	return len(m.Photo) > 0 || m.Voice != nil || m.Audio != nil ||
		m.Video != nil || m.Document != nil || m.Sticker != nil
}

// telegramMessageToIncoming converts the wire format to the
// channel-agnostic IncomingMessage. Used today by the media path
// for friendly fallback; will become the entry to handleMessage as
// the multi-modal refactor lands.
func telegramMessageToIncoming(m *tgMessage) IncomingMessage {
	out := IncomingMessage{
		Text:      m.Text,
		Caption:   m.Caption,
		Channel:   "telegram",
		ChatID:    fmt.Sprintf("%d", m.Chat.ID),
		Timestamp: time.Unix(m.Date, 0),
	}
	if m.From != nil {
		out.UserID = fmt.Sprintf("%d", m.From.ID)
	}
	for _, p := range m.Photo {
		out.Attachments = append(out.Attachments, Attachment{
			Kind:      AttachmentImage,
			MimeType:  "image/jpeg",
			Size:      p.FileSize,
			Width:     p.Width,
			Height:    p.Height,
			Reference: p.FileID,
		})
	}
	if m.Voice != nil {
		out.Attachments = append(out.Attachments, fileMetaToAttachment(m.Voice, AttachmentVoice))
	}
	if m.Audio != nil {
		out.Attachments = append(out.Attachments, fileMetaToAttachment(m.Audio, AttachmentAudio))
	}
	if m.Video != nil {
		out.Attachments = append(out.Attachments, fileMetaToAttachment(m.Video, AttachmentVideo))
	}
	if m.Document != nil {
		out.Attachments = append(out.Attachments, fileMetaToAttachment(m.Document, AttachmentDocument))
	}
	if m.Sticker != nil {
		out.Attachments = append(out.Attachments, fileMetaToAttachment(m.Sticker, AttachmentSticker))
	}
	return out
}

func fileMetaToAttachment(f *tgFileMeta, kind AttachmentKind) Attachment {
	return Attachment{
		Kind:      kind,
		MimeType:  f.MimeType,
		Size:      f.FileSize,
		Duration:  f.Duration,
		Reference: f.FileID,
	}
}

// pollDefaults tune the long-poll loop. Chosen to balance Telegram
// API etiquette (long-poll timeout 25s = a quarter of their 60s
// server-side max) against backoff behaviour on flaky networks.
const (
	pollLongTimeout    = 25 * time.Second
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
//  1. loop getUpdates(offset=next, timeout=25s) — Telegram holds
//     the connection until updates arrive or the timeout expires
//  2. for each update: advance offset, dispatch through the same
//     path the webhook uses (dispatchUpdate)
//  3. on transport error: exponential backoff 1s→30s (factor 1.8)
//  4. on HTTP 409 Conflict: a webhook is registered — call
//     deleteWebhook, then resume. Telegram refuses getUpdates while
//     a webhook is live, so this recovers the stuck state.
//
// Offset is kept in-memory only. A restart re-calls getUpdates with
// offset=0, which returns everything Telegram has buffered
// (< 24h retention). Duplicates are caught by the shared firstSeen
// cache downstream.
func (h *TelegramHandler) RunLongPoll(ctx context.Context) error {
	if h.cfg.Mode != TelegramModePoll {
		return fmt.Errorf("telegram: RunLongPoll called with mode=%q", h.cfg.Mode)
	}
	if h.cfg.Gate != nil {
		// Singleton-gated: poll only while we own the lease. The gate
		// cancels owned() whenever this node loses raft leadership;
		// pollLoop returns and singleton.Run waits for the next gain.
		return singleton.Run(ctx, h.cfg.Gate, "telegram-poll", h.log, h.pollLoop)
	}
	return h.pollLoop(ctx)
}

func (h *TelegramHandler) pollLoop(ctx context.Context) error {
	h.log.Info("telegram: long-poll loop starting")

	var (
		nextOffset = h.loadPersistedOffset(ctx)
		backoff    = pollInitialBackoff
		// firstFlush handles the no-persisted-offset case: rather
		// than re-processing Telegram's 24h buffered backlog (which
		// causes duplicate agent turns + duplicate replies on every
		// restart), do an ack-only first call. We learn the latest
		// update_id, persist it, and start LIVE polling from there.
		// Operators with channel-state-store=nil keep the legacy
		// "process backlog" behaviour for backwards compat.
		firstFlush = nextOffset == 0 && h.cfg.ChannelState != nil
	)
	if nextOffset > 0 {
		h.log.Info("telegram: resuming from persisted offset", "offset", nextOffset)
	} else if firstFlush {
		h.log.Info("telegram: no persisted offset; first-run flush will discard buffered backlog without processing")
	}
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
			if isPollerConflict(err) {
				// Another lobslaw (or any client) is polling the same
				// bot token. We can't fix this — only one long-poller
				// wins. Log loudly once per backoff window and wait.
				h.log.Warn("telegram: another instance is polling this bot token — only one long-poller wins; stop the other process or use a different token",
					"backoff", backoff)
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(backoff):
				}
				backoff = nextBackoff(backoff)
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

		if firstFlush {
			if len(updates) > 0 {
				h.log.Info("telegram: first-run flush discarded buffered updates",
					"count", len(updates), "ack_offset", newOffset)
			}
			firstFlush = false
		} else {
			for i := range updates {
				h.dispatchUpdate(ctx, &updates[i])
			}
		}
		if newOffset > nextOffset {
			nextOffset = newOffset
			h.persistOffset(ctx, nextOffset)
		}
	}
}

// loadPersistedOffset reads the last-known offset from the
// raft-backed channel store. Missing-state or any read error
// returns 0, falling back to the legacy behaviour. Surface non-
// not-found errors at WARN so a misconfigured store is visible.
func (h *TelegramHandler) loadPersistedOffset(ctx context.Context) int64 {
	if h.cfg.ChannelState == nil {
		return 0
	}
	raw, err := h.cfg.ChannelState.Get(ctx, "telegram", "offset")
	if err != nil {
		if !errors.Is(err, types.ErrNotFound) {
			h.log.Warn("telegram: load persisted offset failed; resuming from 0", "err", err)
		}
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		h.log.Warn("telegram: parse persisted offset failed; resuming from 0", "err", err)
		return 0
	}
	return n
}

// persistOffset writes nextOffset via the raft-backed channel store.
// Best-effort: a write failure logs WARN but doesn't abort the
// poll loop. The next successful write covers any missed updates
// because we always persist the LATEST observed offset.
func (h *TelegramHandler) persistOffset(ctx context.Context, nextOffset int64) {
	if h.cfg.ChannelState == nil {
		return
	}
	value := []byte(strconv.FormatInt(nextOffset, 10))
	if err := h.cfg.ChannelState.Put(ctx, "telegram", "offset", value); err != nil {
		h.log.Warn("telegram: persist offset failed", "offset", nextOffset, "err", err)
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

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode == http.StatusConflict {
		desc := strings.ToLower(strings.TrimSpace(string(raw)))
		// Telegram returns 409 in two distinct shapes — webhook conflict
		// is recoverable in-process (we just call deleteWebhook), the
		// other-getUpdates conflict means a second client is polling
		// the same bot token and only the operator can resolve it.
		if strings.Contains(desc, "webhook") {
			return nil, 0, errWebhookConflict
		}
		return nil, 0, fmt.Errorf("%w: %s", errPollerConflict, strings.TrimSpace(string(raw)))
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

// errPollerConflict signals a 409 because a second process is polling
// the same bot token. Telegram only delivers updates to one
// long-poller at a time. We can't recover in-process — back off and
// keep trying so whichever instance the operator stops will let this
// one resume.
var errPollerConflict = errors.New("telegram: getUpdates returned 409 (another instance is polling the same bot token)")

func isWebhookConflict(err error) bool { return errors.Is(err, errWebhookConflict) }
func isPollerConflict(err error) bool  { return errors.Is(err, errPollerConflict) }

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
