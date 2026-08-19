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
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/policy"
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

	// RelatednessJudge is consulted by queue_mode = "smart" to decide
	// whether an arriving message continues the one being collected.
	// Nil makes smart behave as debounce.
	RelatednessJudge RelatednessJudge

	// IncomingDir is where inbound attachments are written. Empty →
	// DefaultIncomingDownloadDir. The vision/audio/pdf builtins read
	// from the SAME value, so a file landing somewhere the agent may
	// not look is not expressible.
	IncomingDir string

	// Notices appends operator notices to outbound replies. Nil
	// disables them entirely, which is what a deployment that never
	// opted this channel in gets.
	Notices *Notices

	// QueueMode and QueueDebounce configure per-conversation turn
	// serialisation. Zero value is QueueSerial, which is both the
	// safe default and the one that drops nothing.
	QueueMode     QueueMode
	QueueDebounce time.Duration

	// Leaser adds cluster-wide turn ownership on top of the in-process
	// queue. Nil is correct for a single node.
	Leaser SessionLeaser

	// ArtifactOpener resolves a produced file's reference to its
	// bytes. Nil means files a turn generates cannot be delivered —
	// the handler says so rather than dropping them silently.
	ArtifactOpener ArtifactOpener

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

	// Roles returns the policy roles the operator declared for a
	// channel user id, from [[user]] roles. Telegram has no token to
	// carry a `roles` claim the way REST does, so without this every
	// turn from this channel is role-less and no rule written against
	// subject = "role:…" can ever match it. Nil → no roles, which is
	// the correct reading of a deployment that declared none.
	Roles func(userID string) []string

	// Identity resolves a Telegram numeric id to the canonical
	// principal an operator bound it to. Nil keeps the channel's own
	// derived id, which is today's behaviour.
	Identity *identity.Resolver

	// Enrolments applies an operator-enrolment decision reached over
	// this channel. Nil disables channel approval; the CLI path still
	// works, so an enrolment is never stranded by this being unset.
	Enrolments EnrolmentDecider

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
	Prompts Prompts

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

	// Sessions is the durable conversation transcript store. Nil
	// leaves the handler on its in-memory buffer — conversations then
	// reset on restart, as they did before sessions existed.
	Sessions SessionStore

	// Compactor folds aged-out conversation into a running summary.
	// Nil disables compaction.
	Compactor SessionCompactor

	// Conversation tunes replay depth and the degraded-mode cache.
	Conversation ConversationConfig

	// Approvals records "approve for the rest of this chat" grants.
	// The executor consults the same instance, so a grant recorded
	// here suppresses the next prompt for that operation. Nil leaves
	// every confirmation one-shot.
	Approvals *compute.SessionApprovals

	// ApprovalRules mints the permanent rule behind "always". Nil
	// hides the button rather than showing one that does nothing —
	// a node without raft has nowhere to record a lasting grant.
	ApprovalRules *policy.ApprovalRules
}

// grantSubject is the policy subject a permanent grant binds to.
//
// Read from the turn's claims rather than from the callback payload:
// the callback is attacker-shaped input, and a subject taken from it
// would let a crafted tap mint a rule for somebody else.
//
// KNOWN HAZARD, and not one "always" introduces: for Telegram this
// identity is the @username when the user has one, which is
// reassignable. A rename orphans the grant (safe), and whoever claims
// the freed handle next inherits it (not safe). The same is true of
// every operator-authored `user:tg-@name` rule and of role assignment,
// so binding approvals to something else would be inconsistent without
// fixing the larger problem. Tracked as its own roadmap item.
// numericSubject is the "user:tg-<id>" form, which config can name
// because the numeric id is stable and visible in the Telegram
// console. Empty when there is no user to attribute.
func numericSubject(u *tgUser) string {
	if u == nil || u.ID == 0 {
		return ""
	}
	return "user:tg-" + strconv.FormatInt(u.ID, 10)
}

func grantSubject(claims *types.Claims) string {
	if claims == nil || claims.UserID == "" || claims.UserID == "tg-unknown" {
		return ""
	}
	return "user:" + claims.UserID
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

	// pendingScope remembers which operation each prompt is about, so
	// an "approve for this chat" tap can record a grant that matches.
	// Keyed by prompt id and drained by the same paths that drain
	// continuations.
	pendingScopeMu sync.Mutex
	pendingScope   map[string]scopedOperation

	// gate serialises turns per conversation. See turnqueue.go — in
	// webhook mode every update lands on its own net/http goroutine,
	// so without it two messages in one conversation run at once.
	gate *TurnGate

	// inflightMu guards the de-dup cache. Telegram retries on
	// network errors; without dedup a tool invocation could run
	// twice for one user intent.
	inflightMu sync.Mutex
	seenUpdate map[int64]time.Time // update_id → first-seen time

	// conv is the per-chat conversation transcript feeding
	// ProcessMessageRequest.ConversationHistory: durable when a
	// session store is wired, in-memory otherwise.
	conv *conversationLog
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
		cfg:          cfg,
		agent:        agent,
		log:          logger,
		client:       client,
		base:         base,
		gate:         NewTurnGate(cfg.QueueMode, cfg.QueueDebounce, logger).WithLeaser(cfg.Leaser, 0).WithJudge(cfg.RelatednessJudge),
		pendingScope: make(map[string]scopedOperation),
		seenUpdate:   make(map[int64]time.Time),
		conv:         newConversationLog(cfg.Sessions, cfg.Compactor, cfg.Conversation, logger),
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
		UserID: h.principalFor(ctx, msg.From),
		Scope:  scope,
	}
	claims.Roles = h.rolesFor(claims.UserID)
	turnID := "tg-" + strconv.FormatInt(msg.MessageID, 10)

	h.log.Debug("telegram: message received",
		"turn_id", turnID,
		"user_id", userIDOf(msg.From),
		"username", usernameOf(msg.From),
		"scope", scope,
		"text_len", len(msg.Text))

	sessionRef := SessionRef{
		Channel:   "telegram",
		ChannelID: strconv.FormatInt(msg.Chat.ID, 10),
		UserID:    claims.UserID,
	}
	// Everything from here to the Append below is one turn, and two
	// of them on the same chat must not overlap: both would Load the
	// same prior history and both would Append it, interleaving the
	// transcript. In webhook mode each update arrives on its own
	// net/http goroutine, so that is the normal case, not a rare one.
	lease, disposition := h.gate.Acquire(ctx, cacheKey(sessionRef), turnID, turnText(msg))
	switch disposition {
	case Folded:
		// Another turn absorbed this message and will answer for it.
		h.log.Debug("telegram: message folded into an in-flight turn",
			"turn_id", turnID, "chat_id", msg.Chat.ID)
		return
	case Dropped:
		h.log.Info("telegram: message dropped by queue policy",
			"turn_id", turnID, "chat_id", msg.Chat.ID, "mode", h.gate.Mode())
		if h.gate.Mode() == QueueOff {
			// Only "off" owes the user an explanation. Under "latest"
			// the message was overtaken by a newer one from the same
			// person, who is about to get an answer anyway.
			h.sendText(msg.Chat.ID, "Still working on your previous message — send that again once I've replied.")
		}
		return
	}
	defer lease.Release()

	prior := h.conv.Load(ctx, sessionRef)
	h.log.Debug("telegram: conversation history loaded",
		"turn_id", turnID,
		"chat_id", msg.Chat.ID,
		"prior_turns", len(prior.Messages),
		"summarised", prior.Summary != "")

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
	// lease.Batch is this message plus anything folded into it while
	// it waited. Using msg alone here would silently drop the
	// fragments the gate promised to answer.
	body := strings.Join(lease.Batch, "\n")
	if body == "" && im.HasMedia() {
		body = "(no caption — please inspect the attached media and respond)"
	}

	agentReq := compute.ProcessMessageRequest{
		Message:             body,
		Attachments:         im.Attachments,
		Claims:              claims,
		TurnID:              turnID,
		Budget:              budget,
		ConversationHistory: prior.Messages,
		ConversationSummary: prior.Summary,
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
		h.sendText(msg.Chat.ID, classifyAgentError(err))
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
	if newTurn := newTurnMessages(resp.Messages, resp.TurnStartIndex); len(newTurn) > 0 {
		h.conv.Append(ctx, sessionRef, turnID, newTurn)
	}

	switch {
	case resp.NeedsConfirmation:
		if h.cfg.Prompts != nil {
			h.sendConfirmationKeyboard(msg.Chat.ID, agentReq, resp, sessionRef)
			return
		}
		// Fallback: no registry wired — render the reason as plain text.
		h.sendText(msg.Chat.ID, "Confirmation required: "+resp.ConfirmationReason)
	case resp.Reply == "":
		h.sendText(msg.Chat.ID, "(empty reply)")
	default:
		// Appended AFTER the transcript was persisted above, and to
		// the outbound text only. A notice recorded as an assistant
		// message is one the model reads next turn and reasons about —
		// at which point the agent is discussing its own pending
		// proposals with the user, and it is in the summary forever.
		// The numeric id travels alongside the principal: a user with
		// a Telegram username is attributed as "tg-@name", which no
		// config file can predict, while the id is what the operator
		// wrote down and what identity resolution is keyed on.
		h.sendText(msg.Chat.ID, h.cfg.Notices.Append(ctx,
			"telegram", sessionRef.ChannelID, grantSubject(claims), resp.Reply,
			numericSubject(msg.From)))
	}
	// After the text: a file the turn produced is context for the
	// reply, not a replacement for it.
	h.SendAttachments(msg.Chat.ID, resp.Attachments, h.cfg.ArtifactOpener)
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
func (h *TelegramHandler) sendConfirmationKeyboard(chatID int64, req compute.ProcessMessageRequest, resp *compute.ProcessMessageResponse, session SessionRef) {
	ttl := h.cfg.ConfirmationTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	// The paused turn rides on the prompt itself. It used to live in a
	// Go map on this handler, which is why an approval after a restart
	// could only tell the user to send it again, and why a tap that
	// reached a different node did nothing at all.
	p, err := h.cfg.Prompts.Create(NewPrompt{
		TurnID:       req.TurnID,
		SessionID:    session.ChannelID,
		Reason:       resp.ConfirmationReason,
		Channel:      "telegram",
		ChannelID:    strconv.FormatInt(chatID, 10),
		TTL:          ttl,
		Action:       resp.ConfirmationAction,
		Resource:     resp.ConfirmationResource,
		Continuation: &Continuation{Request: req, Messages: resp.Messages},
		// Who may answer. Captured here rather than read off the tap,
		// for the same reason the "always" subject is: a callback is
		// attacker-shaped input and the turn that raised the question
		// is not.
		RaisedFor: session.UserID,
	})
	if err != nil {
		h.log.Error("telegram: prompt registration failed", "err", err)
		h.sendText(chatID, "Confirmation required: "+resp.ConfirmationReason)
		return
	}

	buttons := []map[string]string{
		{"text": "Approve", "callback_data": "prompt:approve:" + p.ID},
	}
	// "for this chat" is offered only when a policy rule asked. A
	// budget confirmation is about spend, not an operation, so there
	// is nothing coherent to remember — and a button that silenced
	// future budget warnings would be the last thing an operator
	// wants on that particular prompt.
	if resp.ConfirmationAction != "" && resp.ConfirmationResource != "" {
		subject := grantSubject(req.Claims)
		h.pendingScopeMu.Lock()
		h.pendingScope[p.ID] = scopedOperation{
			action:   resp.ConfirmationAction,
			resource: resp.ConfirmationResource,
			subject:  subject,
		}
		h.pendingScopeMu.Unlock()
		buttons = append(buttons, map[string]string{
			"text": "Approve for this chat", "callback_data": "prompt:approve-session:" + p.ID,
		})
		// "Always" is offered only when there is a principal to bind it
		// to and somewhere to record it. Nil ApprovalRules hides the
		// button rather than showing one that silently does nothing.
		if subject != "" && h.cfg.ApprovalRules != nil {
			buttons = append(buttons, map[string]string{
				"text": "Always allow", "callback_data": "prompt:approve-always:" + p.ID,
			})
		}
	}
	buttons = append(buttons, map[string]string{
		"text": "Deny", "callback_data": "prompt:deny:" + p.ID,
	})

	h.postJSON("sendMessage", map[string]any{
		"chat_id":      chatID,
		"text":         "Confirmation required: " + resp.ConfirmationReason,
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{buttons}},
	})
}

// mayResolve reports whether this tap is allowed to answer this
// prompt.
//
// A prompt id is 128 bits of randomness, which makes it unguessable —
// but unguessable is not the same as authorised. The button is
// rendered into a chat, and in a group every member can see it and
// tap it. Without this check the person who answers a confirmation is
// simply whoever got there first.
//
// The comparison is principal-to-principal rather than raw id to raw
// id, so it survives an `identity rebind` and a renamed handle.
//
// Fails CLOSED. A prompt with no recorded audience is one this node
// cannot attribute an answer to, and guessing in favour of the tapper
// is the wrong way to be wrong about who approved something.
func (h *TelegramHandler) mayResolve(ctx context.Context, promptID string, q *tgCallbackQuery) bool {
	p, err := h.cfg.Prompts.Get(promptID)
	if err != nil || p == nil {
		// Not an authorisation failure — the prompt expired or was
		// reaped. Resolve would say so anyway; this just stops the
		// lookup being repeated.
		return true
	}
	if p.RaisedFor == "" {
		h.log.Warn("telegram: refusing a callback on a prompt with no recorded audience",
			"prompt", promptID)
		h.answerCallback(q, "This confirmation cannot be attributed to anyone; it was not applied.")
		return false
	}
	if q.From == nil {
		h.log.Warn("telegram: refusing an unattributed callback", "prompt", promptID)
		return false
	}
	if !h.isAudience(ctx, q.From, p.RaisedFor) {
		// Logged with both principals: somebody tapping a colleague's
		// confirmation is worth seeing, and it is indistinguishable
		// from an attack in the logs otherwise.
		h.log.Warn("telegram: refusing a callback from somebody the question was not asked of",
			"prompt", promptID, "tapped_by", h.principalFor(ctx, q.From), "raised_for", p.RaisedFor)
		h.answerCallback(q, "That confirmation was not for you.")
		return false
	}
	return true
}

// isAudience reports whether this user is who the question was asked
// of.
//
// Two comparisons, because tgUserIdentity is not stable across
// updates: it prefers the username and falls back to the numeric id,
// so the same person yields "tg-@alice" from a message that carried a
// username and "tg-1" from one that did not. A prompt raised from a
// context with only the id — an enrolment, where nobody has messaged
// us — would otherwise refuse the very person it was raised for.
//
// Widening to the numeric form admits nobody extra: the id is the
// stable identity underneath, and a different user matches neither.
func (h *TelegramHandler) isAudience(ctx context.Context, u *tgUser, raisedFor string) bool {
	if raisedFor == "" || u == nil {
		return false
	}
	if h.principalFor(ctx, u) == raisedFor {
		return true
	}
	return "tg-"+strconv.FormatInt(u.ID, 10) == raisedFor
}

// answerCallback surfaces a refusal on the tapper's own screen.
//
// Silence would read as a broken button and invite retrying, which is
// the worst outcome: the person keeps tapping and the person who can
// actually answer never learns there is a question waiting.
func (h *TelegramHandler) answerCallback(q *tgCallbackQuery, text string) {
	if q.Message == nil {
		return
	}
	h.sendText(q.Message.Chat.ID, text)
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

	if h.cfg.Prompts == nil {
		h.log.Warn("telegram: callback arrived but no prompt registry configured")
		return
	}

	if !h.mayResolve(ctx, promptID, q) {
		return
	}

	var decision PromptDecision
	var scope PromptScope
	var reply string
	switch verb {
	case "approve":
		decision, scope = PromptApproved, PromptScopeOnce
		reply = "Approved."
	case "approve-session":
		decision, scope = PromptApproved, PromptScopeSession
		reply = "Approved — I won't ask again for this in this chat."
		// Recorded before Resolve, so the resumed turn already sees
		// the grant. Resolving first would let the resume race the
		// grant and prompt a second time for the same operation.
		if !h.grantForSession(ctx, promptID, q) {
			decision, scope = PromptApproved, PromptScopeOnce
			reply = "Approved."
		}
	case "approve-always":
		decision, scope = PromptApproved, PromptScopeAlways
		reply = "Approved — I won't ask about this again. Revoke it with `lobslaw policy revoke-approvals`."
		// Recorded before Resolve, for the same reason as the session
		// grant: resolving first lets the resumed turn race the rule
		// and prompt a second time for the same operation.
		if !h.grantAlways(ctx, promptID, q) {
			decision, scope = PromptApproved, PromptScopeOnce
			reply = "Approved."
		}
	case "deny":
		decision, scope = PromptDenied, PromptScopeOnce
		reply = "Denied."
	default:
		h.log.Debug("telegram: unknown prompt verb", "verb", verb, "data", q.Data)
		return
	}

	// Read before resolving. Resolve is a CAS that can lose to another
	// node, and the loser must not consume the turn it did not win.
	prompt, getErr := h.cfg.Prompts.Get(promptID)

	if err := h.cfg.Prompts.Resolve(promptID, decision, scope); err != nil {
		switch {
		case errors.Is(err, ErrPromptNotFound):
			reply = "That prompt no longer exists."
		case errors.Is(err, ErrPromptResolved):
			reply = "That prompt was already resolved."
		default:
			h.log.Error("telegram: resolve failed", "err", err, "id", promptID)
			reply = "Couldn't process the response."
		}
		if q.Message != nil {
			h.sendText(q.Message.Chat.ID, reply)
		}
		return
	}

	if q.Message != nil {
		h.sendText(q.Message.Chat.ID, reply)
	}
	// An enrolment answer goes somewhere other than a paused turn.
	// Handled before the continuation branch below, and for DENIED as
	// well as approved: refusing a request has to actually close it,
	// or the laptop keeps polling a question somebody already said no
	// to.
	if getErr == nil && prompt != nil && prompt.Enrolment != "" {
		h.applyEnrolmentDecision(ctx, prompt, decision == PromptApproved,
			h.principalFor(ctx, q.From))
		return
	}

	if decision == PromptDenied {
		return
	}

	if getErr != nil || prompt == nil || prompt.Continuation == nil {
		// Approved, but there is no turn to resume. Under the old
		// in-process map this was the ordinary outcome of a restart;
		// now it means the prompt was raised without one, or the
		// record was purged between the read and the resolve.
		h.log.Warn("telegram: approve with no continuation",
			"prompt_id", promptID, "err", getErr)
		if q.Message != nil {
			h.sendText(q.Message.Chat.ID, "I've lost track of that turn — send it again.")
		}
		return
	}
	h.resumeAfterApproval(ctx, prompt)
}

// resumeAfterApproval re-enters the agent loop with a relaxed
// budget and sends the final reply (or a new keyboard if another
// confirmation is needed) back to the originating chat. Kept as a
// method so callers can also invoke it from tests.
func (h *TelegramHandler) resumeAfterApproval(ctx context.Context, p *Prompt) {
	cont := p.Continuation
	chatID, err := strconv.ParseInt(p.ChannelID, 10, 64)
	if err != nil {
		h.log.Error("telegram: prompt carries no usable chat id",
			"prompt_id", p.ID, "channel_id", p.ChannelID)
		return
	}
	session := SessionRef{Channel: "telegram", ChannelID: p.SessionID}

	// Tools stay nil: fillDefaults populates them from the resuming
	// node's own registry. Serialising them onto the record would let
	// a definition outlive the redeploy that changed it.
	cont.Request.TurnID = p.TurnID
	cont.Request.Channel = "telegram"
	cont.Request.ChannelID = p.ChannelID

	cont.Request.Budget.Relax()
	resp, err := h.agent.ResumeFromConfirmation(ctx, cont.Request, cont.Messages)
	if err != nil {
		h.log.Error("telegram: resume failed",
			"turn_id", cont.Request.TurnID, "err", err)
		h.sendText(chatID, classifyAgentError(err))
		return
	}

	// Record only what the resumed leg added. Everything up to the
	// confirmation was persisted when the turn first stopped, and
	// ResumeFromConfirmation sets TurnStartIndex to the end of what
	// it was handed — so this appends exactly the new tail.
	if newTurn := newTurnMessages(resp.Messages, resp.TurnStartIndex); len(newTurn) > 0 {
		h.conv.Append(ctx, session, cont.Request.TurnID, newTurn)
	}

	switch {
	case resp.NeedsConfirmation:
		h.sendConfirmationKeyboard(chatID, cont.Request, resp, session)
	case resp.Reply == "":
		h.sendText(chatID, "(empty reply)")
	default:
		h.sendText(chatID, resp.Reply)
	}
	h.SendAttachments(chatID, resp.Attachments, h.cfg.ArtifactOpener)
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

// rolesFor looks up the declared roles for a channel user id. Kept
// separate from resolveScope because the two answer different
// questions: scope is the permission tier the channel assigns, roles
// are what the operator said about the person.
func (h *TelegramHandler) rolesFor(userID string) []string {
	if h.cfg.Roles == nil {
		return nil
	}
	return h.cfg.Roles(userID)
}

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
// principalFor resolves who this message is from, preferring the
// operator's binding for the sender's numeric id over the id Telegram
// hands us.
//
// This is the only place with the raw numeric id, which is why it
// resolves here rather than downstream. tgUserIdentity prefers the
// @username, and a username is reassignable: bound to it, a rename
// orphans somebody's history and grants, and whoever claims the freed
// handle inherits them.
//
// Falls back to tgUserIdentity when nothing is bound, so a deployment
// that declares no [[user]] channels behaves exactly as before and a
// new person can still talk to the bot without an operator editing a
// file first.
func (h *TelegramHandler) principalFor(ctx context.Context, u *tgUser) string {
	fallback := tgUserIdentity(u)
	if h.cfg.Identity == nil || u == nil || u.ID == 0 {
		return fallback
	}
	principal, err := h.cfg.Identity.ResolveChannel(ctx, "telegram",
		strconv.FormatInt(u.ID, 10), fallback)
	if err != nil {
		// Logged, never fatal. A lookup outage must not reassign
		// somebody's identity or lock them out of their own history.
		h.log.Warn("telegram: identity lookup failed; using the channel id",
			"telegram_user_id", u.ID, "err", err)
	}
	if principal.IsZero() {
		return fallback
	}
	return principal.ID()
}

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
			// Acknowledge each update as it completes, not the batch
			// as a whole. The offset is a consumer position: persisting
			// it once at the end means a crash after update 3 of 5
			// leaves it covering none of them, so on restart Telegram
			// redelivers all five and the in-memory dedup map — empty
			// after a restart — lets the first three run a second time.
			// Duplicate replies, duplicate tool calls, duplicate
			// commitments.
			//
			// After dispatch rather than before, so the guarantee stays
			// at-least-once: a crash mid-turn replays that one turn,
			// which is the right trade for a chat bot. Acknowledging
			// first would lose the message instead.
			//
			// One raft write per update rather than per batch. A turn
			// already costs seconds and several writes, so this is
			// noise, and it forwards to the leader like any other write.
			for i := range updates {
				// Do not start a new turn once shutdown has begun.
				// Whatever is left unacknowledged is redelivered on
				// the next start, which is cheaper and safer than
				// beginning work we are about to abandon.
				select {
				case <-ctx.Done():
					return nil
				default:
				}
				h.dispatchUpdate(ctx, &updates[i])
				if ack := updates[i].UpdateID + 1; ack > nextOffset {
					nextOffset = ack
					h.persistOffset(ctx, nextOffset)
				}
			}
		}
		// Covers a batch that dispatched nothing — an update shape we
		// do not handle still has to be acknowledged, or the poller
		// fetches it forever.
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
	defer func() { _ = resp.Body.Close() }()

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
	defer func() { _ = resp.Body.Close() }()
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

// classifyAgentError converts a RunToolCallLoop error into a
// short user-facing message. Generic "Something went wrong" is
// the right fallback only when we genuinely don't know what
// happened — for known patterns (rate limits, all-providers-failed,
// context cancellations) the user gets a more useful nudge so
// they know whether to retry, wait, or chase the operator.
func classifyAgentError(err error) string {
	if err == nil {
		return "Something went wrong processing your message."
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "429"):
		return "I'm hitting an LLM provider rate limit right now. Try again in a minute or two — the limit resets quickly."
	case strings.Contains(msg, "all providers in chain failed"):
		return "All my LLM providers failed on that request. The operator's logs will have the specific reason; usually it's a transient rate-limit or a misconfigured backup. Try again shortly."
	case strings.Contains(msg, "context canceled") || strings.Contains(msg, "deadline exceeded"):
		return "That turn took too long and got cancelled. Could you ask again, maybe with a tighter scope?"
	case strings.Contains(msg, "policy denied"):
		return "Policy blocked that action. The operator's logs have the rule that matched."
	default:
		return "Something went wrong processing your message."
	}
}

// turnText is the message body used both for gating and, after any
// folding, for the turn itself. Caption stands in for text on a
// media-only message so a photo with a caption is not treated as an
// empty message by the queue.
func turnText(msg *tgMessage) string {
	if msg.Text != "" {
		return msg.Text
	}
	return msg.Caption
}

// scopedOperation is the (action, resource) a pending prompt is about.
type scopedOperation struct {
	action   string
	resource string
	// subject is the principal an "always" grant binds to, captured
	// when the prompt was raised rather than read off the callback.
	// A callback is attacker-shaped input; the turn that triggered
	// the confirmation is not.
	subject string
}

// grantAlways mints the permanent policy rule behind "always".
//
// Reports whether a rule was actually recorded, so the reply does not
// promise something that did not happen — the floor refuses grants for
// protected paths and destructive commands, and that refusal must
// reach the user rather than being logged and forgotten.
func (h *TelegramHandler) grantAlways(ctx context.Context, promptID string, q *tgCallbackQuery) bool {
	if h.cfg.ApprovalRules == nil || q.Message == nil {
		return false
	}
	h.pendingScopeMu.Lock()
	op, ok := h.pendingScope[promptID]
	delete(h.pendingScope, promptID)
	h.pendingScopeMu.Unlock()
	if !ok || op.subject == "" {
		return false
	}

	rule, err := h.cfg.ApprovalRules.Mint(ctx, policy.MintRequest{
		PromptID: promptID,
		Subject:  op.subject,
		Action:   op.action,
		Resource: op.resource,
	})
	if err != nil {
		h.log.Warn("telegram: could not mint a permanent approval",
			"action", op.action, "resource", op.resource, "err", err)
		return false
	}
	h.log.Info("telegram: permanent approval recorded",
		"rule_id", rule.Id, "subject", op.subject,
		"action", op.action, "resource", op.resource)
	return true
}

// grantForSession records "approved for the rest of this chat" for the
// operation the prompt was raised about. Reports whether a grant was
// actually recorded, so the reply does not promise something that did
// not happen — no approvals store wired, or a prompt whose operation
// we no longer know.
func (h *TelegramHandler) grantForSession(ctx context.Context, promptID string, q *tgCallbackQuery) bool {
	if h.cfg.Approvals == nil || q.Message == nil {
		return false
	}
	h.pendingScopeMu.Lock()
	op, ok := h.pendingScope[promptID]
	delete(h.pendingScope, promptID)
	h.pendingScopeMu.Unlock()
	if !ok {
		return false
	}

	// The grant is scoped by the conversation on the context, not by
	// anything in the callback payload — the same rule ownership
	// follows, because a callback is attacker-shaped input.
	grantCtx := compute.WithTurnIdentity(ctx, compute.TurnIdentity{
		Channel:   "telegram",
		ChannelID: strconv.FormatInt(q.Message.Chat.ID, 10),
	})
	if !h.cfg.Approvals.Grant(grantCtx, op.action, op.resource) {
		h.log.Warn("telegram: could not record session approval",
			"action", op.action, "resource", op.resource)
		return false
	}
	h.log.Info("telegram: approved for this chat",
		"action", op.action, "resource", op.resource, "chat_id", q.Message.Chat.ID)
	return true
}
