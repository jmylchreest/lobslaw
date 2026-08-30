package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

// ChannelSlack is this channel's kind, as it appears in a SessionRef,
// an [identity.aliases] key prefix and a policy resource.
const ChannelSlack = "slack"

// slackWildcard opens AllowedChannels to every conversation.
const slackWildcard = "*"

// slackDMSentinel matches every direct message in AllowedChannels.
const slackDMSentinel = "dm"

// slackKeepaliveInterval is how often the connection is proved alive,
// and slackPongTimeout is how long a pong may take before the socket
// counts as dead.
//
// A KEEPALIVE rather than a read deadline. The first attempt at this
// bounded Read on the theory that Slack pings every few seconds so
// silence meant death — but coder/websocket answers those pings inside
// Read without making Read return, so an idle-but-healthy socket
// produced no read for a minute and got torn down. Running it showed
// the result immediately: a reconnect every 60s, each one re-running
// auth.test.
const (
	slackKeepaliveInterval = 30 * time.Second
	slackPongTimeout       = 10 * time.Second
)

// SlackConfig wires the Slack channel. Mirrors TelegramConfig field
// for field wherever the two channels genuinely share a concern, so
// that the responsiveness timers, queue policy, session store and
// identity resolution are configured once in wire_gateway.go rather
// than diverging per channel.
type SlackConfig struct {
	// BotToken ("xoxb-…") authenticates every Web API call. AppToken
	// ("xapp-…") opens the Socket Mode connection and does nothing
	// else. Both are required; they are not interchangeable.
	BotToken string
	AppToken string

	// AllowedChannels lists conversation ids this bot will act in.
	// EMPTY IS CLOSED — see the config field's comment. "*" opens every
	// conversation; "dm" opens every direct message.
	//
	// The "dm" sentinel exists because D-ids are minted per user on
	// first contact, so a DM cannot be listed in advance. Without it
	// the only config that let anyone DM the bot was ["*"], which also
	// opened every channel it had been invited to — so an operator
	// wanting a DM-only assistant had to choose between that and a bot
	// nobody could talk to.
	AllowedChannels []string

	// UserScopes maps a Slack user id to a lobslaw scope. Unmapped
	// users fall through to UnknownUserScope, and an empty one drops
	// them. Same contract as Telegram's UserIDScopes — and the same
	// trap: this map, not [[user]], is what grants access.
	UserScopes       map[string]string
	UnknownUserScope string

	// Roles returns policy roles for a resolved principal. Slack, like
	// Telegram, carries no JWT, so without this no rule written
	// against subject = "role:…" can match a Slack turn.
	Roles func(userID string) []string

	// Identity resolves a Slack user id to the canonical principal an
	// operator bound it to.
	Identity *identity.Resolver

	DefaultBudget compute.BudgetCaps

	// Notices, Prompts, Leaser, Sessions, Compactor, Conversation and
	// the queue/responsiveness fields carry the same meaning as their
	// TelegramConfig counterparts.
	Notices         *Notices
	Prompts         Prompts
	ConfirmationTTL time.Duration

	// Approvals records "approve for the rest of this conversation".
	// Nil leaves every confirmation one-shot. ApprovalRules mints the
	// permanent rule behind "always"; nil hides that button rather
	// than offering one that silently does nothing.
	Approvals     *compute.SessionApprovals
	ApprovalRules *policy.ApprovalRules

	Leaser           SessionLeaser
	Sessions         SessionStore
	Compactor        SessionCompactor
	Conversation     ConversationConfig
	QueueMode        QueueMode
	QueueDebounce    time.Duration
	RelatednessJudge RelatednessJudge
	QueueBurstWindow time.Duration
	QueueBurstReset  time.Duration
	TypingInterval   time.Duration
	InterimTimeout   time.Duration
	HardTimeout      time.Duration
	Soul             func() *types.SoulConfig

	// SessionGrants backs /grants. Nil leaves the command
	// unregistered rather than answering "none" on a node that
	// cannot answer at all.
	SessionGrants SessionGrants

	// CommandAuthorizer gates slash commands through the policy engine.
	// Nil refuses every command rather than allowing them: a command
	// is a privileged operation, and an unwired authorizer is an
	// incomplete deployment, not a permissive one.
	CommandAuthorizer CommandAuthorizer

	// ArtifactOpener resolves a produced file's reference to its bytes.
	// Nil means files a turn generates cannot be delivered — the
	// handler says so rather than dropping them silently.
	ArtifactOpener ArtifactOpener

	// IncomingDir is where inbound files are written. Empty →
	// DefaultIncomingDownloadDir, which is the same directory the
	// vision/audio/pdf builtins read from, so a file cannot land
	// somewhere the agent is not allowed to look.
	IncomingDir string

	// SlashPrefix is the umbrella command registered in Slack's app UI.
	// Empty → "lobslaw", so "/lobslaw new" works out of the box and a
	// directly-registered "/new" still dispatches as itself.
	SlashPrefix string

	// Gate restricts the Socket Mode loop to one node. Slack delivers
	// each event to exactly one connection, so two nodes both
	// connected would split a conversation between them at random.
	Gate singleton.Gate

	// HTTPClient overrides the egress-scoped default. Tests inject one
	// pointed at an httptest.Server.
	HTTPClient *http.Client
	// APIBase overrides the Web API root, for the same reason.
	APIBase string

	Logger *slog.Logger
}

// SlackHandler is the Socket Mode receiver. Unlike Telegram's webhook
// half there is no HTTP surface: the connection is outbound, which is
// why this channel needs no public ingress and no request signature
// verification.
type SlackHandler struct {
	cfg   SlackConfig
	agent *compute.Agent
	log   *slog.Logger
	api   *slackAPI

	// identityMu guards botUserID and teamID. They are rewritten every
	// time socketLoop re-authenticates, which singleton.Run does on
	// every regain of ownership — while event goroutines from the
	// previous term may still be reading them.
	identityMu sync.RWMutex
	// botUserID is this bot's own Slack user id, learned from auth.test.
	// Without it the bot answers its own messages, which in a channel is
	// an unbounded loop rather than a cosmetic bug.
	botUserID string
	// teamID is the workspace this bot is installed in, from auth.test.
	// Used only as a fallback: an event carries its own team id, and
	// preferring that keeps a shared-channel event attributed to the
	// workspace it came from. Without the fallback an event that
	// omitted one would produce an UNSCOPED principal, quietly merging
	// two workspaces' identities — which slackUserIdentity exists to
	// prevent.
	teamID string

	// gate serialises turns per conversation, as Telegram's does.
	gate *TurnGate

	// pendingScope remembers which operation each prompt is about, so
	// an "approve here" tap records a grant that matches. Keyed by
	// prompt id and drained when the tap arrives.
	pendingScopeMu sync.Mutex
	pendingScope   map[string]scopedOperation

	// seen de-duplicates redelivered events. Slack redelivers an
	// envelope it never saw acked, and a dropped ack is normal during
	// a reconnect — so without this a reconnect can re-run a turn that
	// already ran, tool calls and all.
	seenMu sync.Mutex
	seen   map[string]time.Time

	conv *conversationLog

	// channels memoises name→id so the read tools can accept the
	// "#general" a model naturally writes without paying a full
	// conversations.list walk per call.
	channels channelCache

	// commands is the shared runtime control surface. Built here rather
	// than handed in so it can close over conv, which stays private.
	commands *CommandSet

	// keepaliveEvery and pongWithin override the package defaults.
	// Zero means the default. Set only by tests, which cannot wait
	// thirty seconds to find out that a quiet socket survived.
	keepaliveEvery time.Duration
	pongWithin     time.Duration
}

func (h *SlackHandler) keepaliveInterval() time.Duration {
	if h.keepaliveEvery > 0 {
		return h.keepaliveEvery
	}
	return slackKeepaliveInterval
}

func (h *SlackHandler) pongTimeout() time.Duration {
	if h.pongWithin > 0 {
		return h.pongWithin
	}
	return slackPongTimeout
}

// NewSlackHandler constructs the handler. Both tokens are required at
// construction: a Slack channel missing either is not a degraded
// channel, it is one that silently never receives or never replies.
func NewSlackHandler(cfg SlackConfig, agent *compute.Agent) (*SlackHandler, error) {
	if cfg.BotToken == "" {
		return nil, errors.New("slack: BotToken required")
	}
	if cfg.AppToken == "" {
		return nil, errors.New("slack: AppToken required for Socket Mode")
	}
	if agent == nil {
		return nil, errors.New("slack: agent required")
	}
	client := cfg.HTTPClient
	if client == nil {
		// Same reasoning as Telegram's: pin outbound traffic to a
		// role whose ACL names Slack's hosts, so a compromised process
		// cannot redirect the bot's traffic somewhere else.
		base := egress.For("gateway/slack").HTTPClient()
		wrapped := *base
		wrapped.Timeout = 30 * time.Second
		client = &wrapped
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if len(cfg.AllowedChannels) == 0 {
		// Not an error — a closed channel is a valid, if useless,
		// configuration — but it is almost always a mistake, and the
		// symptom (every message silently dropped) looks identical to
		// a broken connection.
		logger.Warn("slack: allowed_channels is empty; every conversation will be refused. Set allowed_channels = [\"*\"] to open it.")
	}
	h := &SlackHandler{
		cfg:   cfg,
		agent: agent,
		log:   logger,
		api:   newSlackAPI(cfg.BotToken, cfg.APIBase, client),
		gate: NewTurnGate(cfg.QueueMode, cfg.QueueDebounce, logger).
			WithLeaser(cfg.Leaser, 0).
			WithJudge(cfg.RelatednessJudge).
			WithBurst(cfg.QueueBurstWindow, cfg.QueueBurstReset),
		seen:         make(map[string]time.Time),
		pendingScope: make(map[string]scopedOperation),
		conv:         newConversationLog(cfg.Sessions, cfg.Compactor, cfg.Conversation, logger),
	}
	h.commands = NewCommandSet(cfg.CommandAuthorizer, logger)
	RegisterBuiltinCommands(h.commands, h.conv)
	// Nil leaves /grants unregistered — see RegisterGrantCommands.
	RegisterGrantCommands(h.commands, cfg.SessionGrants)
	return h, nil
}

// ChannelType satisfies the notify.Sink half of this channel.
func (h *SlackHandler) ChannelType() string { return ChannelSlack }

// --- connection lifecycle --------------------------------------------

// RunSocketMode maintains the Socket Mode connection until ctx ends.
//
// Reconnect is normal operation, not error handling: Slack cycles a
// socket roughly hourly and says so with a "disconnect" envelope
// first. Each reconnect calls apps.connections.open again because the
// WSS url is single-use.
func (h *SlackHandler) RunSocketMode(ctx context.Context) error {
	if h.cfg.Gate != nil {
		return singleton.Run(ctx, h.cfg.Gate, "slack-socket", h.log, h.socketLoop)
	}
	return h.socketLoop(ctx)
}

func (h *SlackHandler) socketLoop(ctx context.Context) error {
	// Identity is learned INSIDE the loop, so a failure takes the same
	// backoff a dropped socket does.
	//
	// It used to be a single call above the loop whose error was
	// returned. singleton.Run returns on any non-context error and
	// never re-invokes fn, and the caller logs one Warn — so a network
	// blip or a rate-limited auth.test at boot left Slack silently dead
	// until the process was restarted, while the reconnect path
	// directly below was careful about exactly that.
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		userID, teamID, authErr := h.api.authTest(ctx)
		if authErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			h.log.Warn("slack: auth.test failed; retrying", "err", authErr, "in", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff)
			continue
		}
		h.setIdentity(userID, teamID)
		h.log.Info("slack: authenticated", "bot_user_id", userID, "team_id", teamID)

		err := h.runOneConnection(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			// Clean cycle at Slack's request. Reconnect immediately
			// and reset the backoff: this is the expected path, and
			// treating it as a failure would slowly starve a
			// long-running node of its connection.
			backoff = time.Second
			continue
		default:
			h.log.Warn("slack: socket closed; reconnecting",
				"err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// runOneConnection opens a socket and serves it until Slack asks us to
// disconnect (nil) or it fails (error).
func (h *SlackHandler) runOneConnection(ctx context.Context) error {
	url, err := h.api.openConnection(ctx, h.cfg.AppToken)
	if err != nil {
		return err
	}
	sock, err := dialSlackSocket(ctx, url, h.api.client)
	if err != nil {
		return err
	}
	defer sock.close()
	h.log.Info("slack: socket connected")

	// Liveness runs beside the read rather than bounding it. A failed
	// ping closes the connection, which is what makes the blocked Read
	// below return so the reconnect path can take over.
	connCtx, stopKeepalive := context.WithCancel(ctx)
	defer stopKeepalive()
	go func() {
		t := time.NewTicker(h.keepaliveInterval())
		defer t.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-t.C:
				pingCtx, cancel := context.WithTimeout(connCtx, h.pongTimeout())
				err := sock.ping(pingCtx)
				cancel()
				if err == nil {
					continue
				}
				if connCtx.Err() != nil {
					return
				}
				h.log.Warn("slack: keepalive ping failed; closing the socket", "err", err)
				// closeNow, not close: the peer has stopped answering,
				// so waiting for it to complete a closing handshake
				// would block the very call that is meant to unblock
				// the Read below.
				sock.closeNow()
				return
			}
		}
	}()

	for {
		env, err := sock.read(ctx)
		if err != nil {
			return err
		}
		switch env.Type {
		case "hello":
			h.log.Debug("slack: socket hello")
			continue
		case "disconnect":
			h.log.Info("slack: disconnect requested", "reason", env.Reason)
			return nil
		}

		// Ack BEFORE the turn. Slack allows three seconds and a turn
		// takes tens of them, so an ack that waited for the answer
		// would guarantee redelivery of every message the bot is
		// currently working on.
		if err := sock.ack(ctx, env.EnvelopeID); err != nil {
			return fmt.Errorf("slack: ack: %w", err)
		}
		h.dispatchEnvelope(ctx, env)
	}
}

// dispatchEnvelope routes one acked envelope. Runs the turn on its own
// goroutine so a slow turn does not stall the read loop — the socket
// must keep acking while an answer is being produced.
func (h *SlackHandler) dispatchEnvelope(ctx context.Context, env *slackEnvelope) {
	switch env.Type {
	case "events_api":
		var p slackEventsPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			h.log.Warn("slack: malformed events payload", "err", err)
			return
		}
		// runCtx, not context.WithoutCancel(ctx): a turn started under
		// this node's leadership must stop when that leadership goes,
		// or the old leader keeps answering into channels the new one
		// is already serving.
		go h.handleEvent(ctx, p)
	case "interactive":
		var in slackInteraction
		if err := json.Unmarshal(env.Payload, &in); err != nil {
			h.log.Warn("slack: malformed interaction payload", "err", err)
			return
		}
		// Same context choice as an event, and for the same reason: an
		// approval tap re-enters the agent loop, so it is a turn.
		go h.handleInteraction(ctx, in)
	case "slash_commands":
		var sc slackSlashCommand
		if err := json.Unmarshal(env.Payload, &sc); err != nil {
			h.log.Warn("slack: malformed slash command payload", "err", err)
			return
		}
		go h.handleSlashCommand(ctx, sc)
	default:
		h.log.Debug("slack: unhandled envelope type", "type", env.Type)
	}
}

// --- events ----------------------------------------------------------

func (h *SlackHandler) handleEvent(ctx context.Context, p slackEventsPayload) {
	ev := p.Event
	if !h.wantsEvent(ev) {
		return
	}
	if !h.firstSeen(eventKey(p.TeamID, ev)) {
		h.log.Debug("slack: duplicate event ignored", "ts", ev.TS, "channel", ev.Channel)
		return
	}
	h.handleMessage(ctx, h.teamOr(p.TeamID), ev)
}

// wantsEvent filters the firehose down to messages a human addressed
// to us. Everything rejected here is rejected for a distinct reason,
// and collapsing them would make the loop's behaviour unexplainable.
func (h *SlackHandler) wantsEvent(ev slackEvent) bool {
	switch ev.Type {
	case "message", "app_mention":
	default:
		return false
	}
	// A subtype means an edit, a join, a deletion, a bot post — an
	// event ABOUT a message rather than a new one. Answering
	// message_changed would re-run a turn nobody resent.
	//
	// "file_share" is the exception, and it is not a rare one: it is
	// how EVERY file upload arrives. Rejecting it wholesale would mean
	// the bot silently ignores every screenshot anybody sends it.
	if ev.Subtype != "" && ev.Subtype != "file_share" {
		return false
	}
	// Our own messages, and any other bot's. Without this the bot
	// answers itself, which in a channel does not terminate.
	if ev.BotID != "" || ev.User == "" || ev.User == h.selfUserID() {
		return false
	}
	// Text OR files. A file shared with no comment is a real message —
	// "look at this" is implied by the act of sharing it.
	if strings.TrimSpace(ev.Text) == "" && len(ev.Files) == 0 {
		return false
	}
	return true
}

// eventKey identifies one event for de-duplication. The message
// timestamp is unique per channel and stable across redeliveries,
// which the envelope id is not.
func eventKey(teamID string, ev slackEvent) string {
	return teamID + ":" + ev.Channel + ":" + ev.TS
}

// firstSeen reports whether this is the first delivery of an event,
// and records it. Mirrors the Telegram update-id cache, including its
// eviction: without one a long-lived node's map grows without bound.
func (h *SlackHandler) firstSeen(key string) bool {
	const ttl = 10 * time.Minute
	now := time.Now()

	h.seenMu.Lock()
	defer h.seenMu.Unlock()
	if _, dup := h.seen[key]; dup {
		return false
	}
	for k, at := range h.seen {
		if now.Sub(at) > ttl {
			delete(h.seen, k)
		}
	}
	h.seen[key] = now
	return true
}

// --- authorisation ----------------------------------------------------

// isAllowedChannel gates which conversations produce turns.
//
// Empty means closed. That is the opposite of the usual Go zero-value
// reading and it is deliberate: the alternative is that an operator
// who adds a Slack channel block and forgets the allowlist gets a bot
// that answers in every conversation it was ever invited to.
func (h *SlackHandler) isAllowedChannel(channelID string) bool {
	for _, c := range h.cfg.AllowedChannels {
		c = strings.TrimSpace(c)
		if c == slackWildcard {
			return true
		}
		// "dm" covers every 1:1 conversation, because those cannot be
		// enumerated: Slack mints a D-id per user on first contact, so
		// there is nothing for an operator to write down in advance.
		// Without it the only working config for a DM-capable bot was
		// ["*"], which also opens every channel the bot is in — the
		// opposite of what somebody listing conversations intends.
		if strings.EqualFold(c, slackDMSentinel) {
			if slackChannelIsDM(channelID) {
				return true
			}
			continue
		}
		if strings.EqualFold(c, channelID) {
			return true
		}
	}
	return false
}

// resolveScope maps a Slack user id to a scope. Same shape and same
// contract as the Telegram handler's, including the silent drop.
func (h *SlackHandler) resolveScope(userID string) (string, bool) {
	if scope, ok := h.cfg.UserScopes[userID]; ok {
		return scope, true
	}
	if h.cfg.UnknownUserScope != "" {
		return h.cfg.UnknownUserScope, true
	}
	return "", false
}

// slackUserIdentity is the channel's own name for a user, used when no
// operator binding exists. Namespaced by team because the same person
// in two workspaces is two different accounts, and one principal
// spanning both would merge two sets of memories that were never
// meant to meet.
func slackUserIdentity(teamID, userID string) string {
	if userID == "" {
		return "slack-unknown"
	}
	if teamID == "" {
		return "slack-" + userID
	}
	return "slack-" + teamID + "-" + userID
}

func (h *SlackHandler) principalFor(ctx context.Context, teamID, userID string) string {
	fallback := slackUserIdentity(teamID, userID)
	if h.cfg.Identity == nil || userID == "" {
		return fallback
	}
	principal, err := h.cfg.Identity.ResolveChannel(ctx, ChannelSlack, userID, fallback)
	if err != nil {
		h.log.Warn("slack: identity lookup failed; using the channel id",
			"slack_user_id", userID, "err", err)
	}
	if principal.IsZero() {
		return fallback
	}
	return principal.ID()
}

// slackChannelSubject is the policy-subject form of a Slack user's
// channel-derived id, for the notice subject allowlist.
//
// The counterpart of Telegram's numericSubject: an operator writes down
// the id they can see, which is the raw Slack handle, while a bound
// user's principal is whatever the alias maps to. Offering both is what
// stops a configured allowlist silently matching nobody.
func slackChannelSubject(teamID, userID string) string {
	if userID == "" {
		return ""
	}
	return "user:" + slackUserIdentity(teamID, userID)
}

// teamOr resolves the workspace for an event, preferring the one the
// event carried.
//
// Read under the lock. The old comment here said these were "written
// once at connect, before any event goroutine exists, so no lock is
// needed" — true on the first acquire and false afterwards.
// singleton.Run re-invokes socketLoop on every ownership regain, which
// rewrites both fields, while event goroutines spawned from a previous
// term are still reading them.
func (h *SlackHandler) teamOr(eventTeam string) string {
	if eventTeam != "" {
		return eventTeam
	}
	h.identityMu.RLock()
	defer h.identityMu.RUnlock()
	return h.teamID
}

// setIdentity records who this bot is on the workspace it just
// authenticated against.
func (h *SlackHandler) setIdentity(userID, teamID string) {
	h.identityMu.Lock()
	h.botUserID, h.teamID = userID, teamID
	h.identityMu.Unlock()
}

// selfUserID is this bot's own user id, used to drop its own messages
// and to strip its mention out of a body.
func (h *SlackHandler) selfUserID() string {
	h.identityMu.RLock()
	defer h.identityMu.RUnlock()
	return h.botUserID
}

func (h *SlackHandler) rolesFor(userID string) []string {
	if h.cfg.Roles == nil {
		return nil
	}
	return h.cfg.Roles(userID)
}

// --- conversation shape -----------------------------------------------

// slackThreadSep joins a channel to a thread inside one conversation
// id.
//
// NOT ':'. The session store builds its bolt key range as
// "<channel>:<channel_id>:<seq>" and rejects a channel_id containing a
// colon outright — without that rule one conversation's key range
// would overlap another's. A colon here therefore does not fail
// loudly: durable session writes are refused and every Slack thread
// silently degrades to in-memory history that dies with the process.
const slackThreadSep = "/"

// slackConversationID is the key a Slack conversation is stored under.
//
// A thread gets its own: replies in a thread are a separate
// conversation from the channel they hang off, and keying both on the
// channel would interleave every thread into one transcript. This is
// what gives threads their own memory without any thread-specific
// machinery.
//
// Shared by the turn path and the approval path, which must agree: a
// grant recorded under one spelling and looked up under another is a
// confirmation the user answers twice.
func slackConversationID(channel, threadTS string) string {
	if threadTS != "" {
		return channel + slackThreadSep + threadTS
	}
	return channel
}

// The conversation is wherever the reply lands, which is why this is
// derived FROM replyThread rather than alongside it.
//
// Reading ev.ThreadTS directly was wrong for a channel, and split every
// thread in half. A top-level mention carries no thread_ts, so the turn
// was keyed "C1" — but the answer went into a thread rooted at that
// message, so the user's follow-up arrived carrying thread_ts and keyed
// "C1/<ts>". First exchange in one session, the rest in another, and
// the bot could not remember the message that started the thread it was
// standing in.
//
// A DM answers inline, so its conversation is the channel and stays
// that way. An explicitly threaded DM is genuinely its own
// conversation, and replyThread returns empty there — hence the branch
// rather than one expression.
func conversationID(ev slackEvent) string {
	if ev.ChannelType == "im" {
		return slackConversationID(ev.Channel, ev.ThreadTS)
	}
	return slackConversationID(ev.Channel, replyThread(ev))
}

// isSharedSlackConversation reports whether more than one person can
// read this conversation, which decides what passive recall may
// surface into it (see memory.ForConversation).
//
// "im" is a 1:1 DM. Everything else — channel, group, mpim — has an
// audience. An UNKNOWN channel_type counts as shared, for the same
// asymmetry the Telegram equivalent documents: under-sharing costs
// recall, over-sharing discloses somebody's memories to a room.
func isSharedSlackConversation(ev slackEvent) bool {
	return ev.ChannelType != "im"
}

// replyThread decides where an answer goes.
//
// In a DM, the channel. Anywhere else, a thread — hanging off the
// existing thread when there is one, and off the triggering message
// when there is not. Replying inline to a channel would put the bot's
// answers in everyone's main view; a thread keeps a conversation with
// the bot legible to the people not in it.
func replyThread(ev slackEvent) string {
	if ev.ChannelType == "im" {
		return ""
	}
	if ev.ThreadTS != "" {
		return ev.ThreadTS
	}
	return ev.TS
}

// statusThread addresses the assistant thread the native status
// decorates, which is NOT the same question as where a reply goes.
//
// A DM replies inline — there is no room to keep tidy — so its reply
// thread is empty. The assistant thread is not: it is rooted at the
// user's own message. Reading the reply thread for both meant the
// status was skipped for want of a thread in the single surface where
// assistant threads actually exist, and every DM fell back to posting
// a placeholder. Never empty, so a caller cannot repeat that.
func statusThread(ev slackEvent) string {
	if ev.ThreadTS != "" {
		return ev.ThreadTS
	}
	return ev.TS
}
