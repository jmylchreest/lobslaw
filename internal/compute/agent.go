package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/promptguard"
	"github.com/jmylchreest/lobslaw/internal/trace"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Sentinel errors surfaced by RunToolCallLoop. Callers branch on
// these to distinguish transient failures (retry-safe) from
// terminal ones (surface to user).
var (
	// ErrBudgetExceeded fires when the TurnBudget trips during the
	// loop. The returned ProcessMessageResponse carries
	// NeedsConfirmation=true so channel handlers can prompt the user
	// to approve continuation.
	ErrBudgetExceeded = errors.New("agent: turn budget exceeded")

	// ErrMaxToolLoops fires when the loop hits MaxToolLoops without
	// the LLM giving a text-only response. Protects against models
	// that get stuck in tool-call loops.
	ErrMaxToolLoops = errors.New("agent: max tool-call loops reached")

	// ErrNoLLMProvider fires when Agent is constructed without a
	// Provider. Explicit error so tests / wiring bugs surface loudly.
	ErrNoLLMProvider = errors.New("agent: no LLM provider configured")
)

// DefaultMaxToolLoops is the cap on how many tool-call round trips
// one turn may perform. Prevents an infinite-tool-call bug (model
// keeps calling tools without ever emitting text) from burning the
// whole budget. Operators override via AgentConfig if a specific
// workflow legitimately needs more rounds.
//
// 24 chosen empirically: 'fetch a repo + walk a few directories +
// read a few files + synthesise' tops out around 16-20 fetch_url
// calls in practice. 16 was too tight; the bot was hitting the
// wall mid-research with useful state but nowhere to go. 24 gives
// genuine multi-file research turns room to land while still
// catching the broken-loop pathology before it racks up tokens.
const DefaultMaxToolLoops = 24

// AgentConfig configures the agent loop.
type AgentConfig struct {
	// Provider is the LLM the agent calls. Required — nil yields
	// ErrNoLLMProvider at Run time.
	Provider LLMProvider

	// SummaryTimeout bounds the graceful reply produced after a hard
	// timeout. That reply needs a context the expired one cannot
	// cancel, which means a provider that has stopped responding is
	// re-entered with a fresh one — so it must be bounded, or the
	// timeout is defeated by the code meant to report it.
	//
	// Zero takes defaultSummaryReplyTimeout. Worth lowering alongside
	// a short gateway HardTimeout: a 15s tail on a 30s cap is most of
	// the budget again.
	SummaryTimeout time.Duration

	// Health remembers which providers recently failed, so a chain
	// skips one that is in cooldown instead of paying a round-trip to
	// rediscover it every turn. Nil reports everything healthy, which
	// is exactly the behaviour before it existed.
	Health *ProviderHealth

	// Executor runs tool invocations. Required for any turn that
	// involves tool calls (i.e. most of them).
	Executor *Executor

	// Resolver picks which chain a turn runs on, from
	// [[compute.chains]] triggers and the preflight's judgment. Nil
	// starts every turn at PrimaryLabel, which is what happened before
	// chains routed at all.
	Resolver *Resolver

	// Judge produces the routing signal — a complexity score, domain
	// tags and a hint — from a cheap preflight model. Nil is usable
	// and yields the neutral judgment, so only `always` and
	// hint-labelled chains can match.
	Judge *Judge

	// Registry supplies the tool list advertised to the LLM on
	// every turn. Channels shouldn't each have to know to plumb
	// this — the agent pulls its own tool list at turn start. Nil
	// → no tools are advertised (model runs without function-
	// calling unless the caller populates req.Tools manually).
	Registry *Registry

	// Soul returns the current SoulConfig on each turn. Agent
	// assembles the system prompt via promptgen so channels stay
	// transport-only. Callback (not snapshot) so SOUL.md hot-
	// reload takes effect on the next turn without rebuilding the
	// agent. Nil → no system prompt is injected unless the caller
	// populates req.SystemPrompt manually.
	Soul func() *types.SoulConfig

	// EpisodicIngester, when non-nil, receives each turn's
	// user-message + assistant-reply pair as an EpisodicRecord
	// write opportunity. The agent calls IngestTurn after a
	// successful reply — nothing ingested on confirmation-pending
	// or error paths. Dream consolidation picks up what lands here.
	EpisodicIngester EpisodicIngester

	// Roles is the multi-provider map so non-main workloads
	// (preflight classification, reranker, summariser) can target
	// a different model than the primary turn. Nil → every role
	// falls through to Provider.
	Roles *RoleMap

	// Traces records what this turn did. Nil turns tracing off, and a
	// nil recorder is usable, so no call site branches on it.
	Traces *trace.Recorder

	// SelfLearningMode is the [self_learning] mode, surfaced in the
	// system prompt so the assistant can answer truthfully about its
	// own configuration. Empty when off.
	SelfLearningMode string

	// PrimaryLabel names the provider that maps to Provider above
	// in the registry. Used as the starting point for backup-chain
	// walks. Empty → no chain walk, single-provider behaviour.
	PrimaryLabel string

	// Providers supplies label-keyed lookup for backup-chain
	// fallback + council tools. nil → single-provider mode
	// (Provider is the only LLM path).
	Providers *ProviderRegistry

	// Identity resolves per-channel user ids to cluster-wide
	// principals. Nil resolves every id to itself, which is correct
	// for a deployment that has declared no aliases.
	Identity *identity.Resolver

	// ContextEngine, when non-nil, runs per-turn semantic memory
	// recall and appends a "Relevant context" block to the
	// system prompt. Channels don't see or configure this —
	// it's the agent's job to enrich the turn.
	ContextEngine *ContextEngine

	// Hooks dispatches lifecycle events (PreLLMCall, PostLLMCall,
	// PreToolUse, PostToolUse). May be nil — all hook calls become
	// no-ops when unset.
	Hooks HookDispatcher

	// MaxToolLoops bounds tool-call round-trips per turn. 0 →
	// DefaultMaxToolLoops.
	MaxToolLoops int

	// ContextBudget bounds how much prior conversation is replayed
	// into each turn. Lives on the agent rather than the channel so
	// every entry point — REST, Telegram, scheduled turns — is
	// bounded by the same policy. Zero value disables both knobs;
	// the node wires DefaultContextBudget() when config omits the
	// section.
	ContextBudget ContextBudget

	// Skills routes tool-call names that match a registered skill
	// through the skill invoker instead of the tool Executor. Nil
	// disables skill dispatch — every tool-call goes through the
	// executor. The interface is narrow on purpose: the agent
	// shouldn't know what a manifest is.
	Skills SkillDispatcher

	// TimezoneResolver returns the IANA zone the user prefers for
	// time rendering. Resolved per turn from the user's prefs
	// bucket, falling back to the cluster default. Empty result
	// (or nil resolver) leaves UserTimezone empty and time output
	// stays UTC. Wired by the node to read BucketUserPrefs.
	TimezoneResolver func(userID string) string

	// BinariesProvider returns the operator-declared [[binary]]
	// catalogue to advertise in the system prompt every turn. The
	// callback runs per-turn so install/uninstall and help-capture
	// updates take effect without an agent restart. Nil → no Host
	// Binaries section is rendered.
	BinariesProvider func() []promptgen.BinaryInfo

	// PinnedProvider supplies the always-on memory blocks for a
	// session.
	//
	// Called with a session key and expected to return the SAME
	// snapshot for the life of that session. A block that changed
	// mid-session would invalidate the provider's prompt cache on the
	// turn after every write — the opposite of what an always-on block
	// is for. Writes are durable immediately; the rendered snapshot
	// refreshes at the next session boundary.
	PinnedProvider func(sessionKey, userID string) promptgen.PinnedBlocks

	// Review is the post-turn fork that decides whether a turn taught
	// anything worth keeping. Nil disables it entirely — which is what
	// self-learning being off looks like from here, since the node
	// wires no fork when there is no store for it to write to.
	Review *ReviewFork

	// SkillsProvider supplies the skill index for the system prompt.
	//
	// Without it the "Installed Skills" section renders "(none
	// installed)" on every turn no matter what is installed — which
	// is what it did, so a skill could only ever be invoked by a model
	// that guessed its name.
	SkillsProvider func() []promptgen.SkillInfo

	// ProposalsProvider counts the self-taught artefacts awaiting this
	// owner's approval, for the Installed Skills section.
	//
	// Owner-scoped and therefore per-turn, which is why it takes an
	// argument where SkillsProvider does not — same shape as
	// PinnedProvider. Nil when self-learning is off, and then the
	// section says nothing about proposals rather than saying zero:
	// "none pending" and "the feature is not running" are different
	// statements and only one of them is true.
	ProposalsProvider func(owner string) int

	// Logger is used for structured log entries. Nil → slog.Default().
	Logger *slog.Logger
}

// EpisodicIngester writes per-turn records into episodic memory.
// The agent doesn't talk to Raft directly; implementations behind
// this interface (typically internal/memory) propose the write via
// consensus and swallow routine errors (log level).
type EpisodicIngester interface {
	IngestTurn(ctx context.Context, turn EpisodicTurn) error
}

// EpisodicTurn is one complete user↔assistant exchange ready for
// episodic commit. Channel carries its own identity (channel,
// chat_id, user_id) so dream can cluster by conversation without
// needing message-layer metadata.
type EpisodicTurn struct {
	Channel string
	ChatID  string
	UserID  string
	// Owner is the canonical principal the memory belongs to, rendered
	// as a string to keep this interface free of a memory-layer import.
	// Empty for an anonymous turn, which produces an unowned record.
	Owner       string
	UserMessage string
	AssistReply string
	TurnID      string
	CompletedAt time.Time
}

// SkillDispatcher abstracts the skill invoker so the agent doesn't
// depend on internal/skills directly. internal/skills.Invoker
// satisfies this via a thin adapter in that package.
type SkillDispatcher interface {
	// Has reports whether name is a registered skill. Returning false
	// sends the tool call through the normal Executor path.
	Has(name string) bool
	// Invoke runs the skill. An error is reserved for invocation
	// failures (skill missing, storage label unresolvable, sandbox
	// install failure); non-zero subprocess exits come back via
	// Result.ExitCode so the agent can surface them as tool output.
	Invoke(ctx context.Context, req SkillInvokeRequest) (*SkillInvokeResult, error)
}

// SkillInvokeRequest is what the agent hands the skill dispatcher.
// Mirrors the tool-call shape so the two paths are interchangeable
// from the caller's perspective.
type SkillInvokeRequest struct {
	Name   string
	Params map[string]any
	Claims *types.Claims
	TurnID string
}

// SkillInvokeResult is the subprocess outcome. Matches the relevant
// subset of compute.InvokeResult so runToolCall can treat the two
// paths uniformly.
type SkillInvokeResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// HookDispatcher abstracts the PreLLMCall / PostLLMCall hook events
// the agent loop fires around each LLM round-trip. Kept as an
// interface so the agent package doesn't depend on internal/hooks
// directly; hooks.Dispatcher satisfies this naturally.
//
// PreToolUse / PostToolUse hooks are already dispatched inside
// Executor.Invoke — the agent doesn't re-dispatch them.
type HookDispatcher interface {
	Dispatch(ctx context.Context, event types.HookEvent, payload map[string]any) (*HookResponse, error)
}

// HookResponse mirrors hooks.Response's shape without importing it.
// Callers who want the full Response type use hooks.Dispatcher
// directly; this struct carries what the agent needs.
type HookResponse struct {
	Decision string
	Reason   string
}

// Agent is the agent loop implementation. Stateless per turn; a
// single Agent instance handles every turn on a node.
type Agent struct {
	cfg AgentConfig

	// belowFloorReported dedupes the trust-floor exclusion warning.
	// The condition holds for as long as the config does, and a line
	// repeated every turn is one an operator filters out — including
	// the first time it would have told them something.
	belowFloorReported sync.Map
}

// SetReview attaches the post-turn review fork.
//
// A setter rather than a constructor field because the fork routes
// through the RoleMap the agent's own construction builds, so it
// cannot exist yet when the agent is made. Nil leaves reviews off.
func (a *Agent) SetReview(f *ReviewFork) { a.cfg.Review = f }

// SetSkillDispatcher swaps the skill dispatcher post-construction.
// Used by node wiring to swap in a SkillDispatcherChain once MCP
// servers have started (their tools aren't known at agent-
// construction time — they arrive after tools/list round-trips).
// Safe to call; AgentConfig isn't read concurrently with this
// assignment during normal startup ordering.
func (a *Agent) SetSkillDispatcher(d SkillDispatcher) {
	a.cfg.Skills = d
}

// NewAgent validates required deps and constructs the Agent. Fails
// fast on missing Provider — tests that need to exercise the
// Executor-only path still need a mock provider.
func NewAgent(cfg AgentConfig) (*Agent, error) {
	if cfg.Provider == nil {
		return nil, ErrNoLLMProvider
	}
	if cfg.MaxToolLoops <= 0 {
		cfg.MaxToolLoops = DefaultMaxToolLoops
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Agent{cfg: cfg}, nil
}

// ProcessMessageRequest is the per-turn input.
type ProcessMessageRequest struct {
	// Message is the user's text for this turn.
	Message string

	// Claims identifies the user (for policy evaluation + audit).
	Claims *types.Claims

	// TurnID is a stable identifier for this turn; propagated
	// through request IDs in logs + audit.
	TurnID string

	// Channel + ChannelID identify the gateway origin of the turn.
	// Threaded into the assembled system prompt so the agent can
	// address proactive replies via the channel-agnostic notify
	// builtin when storing prompts in commitments / scheduled
	// tasks. Empty for internally-originated turns (scheduler).
	Channel   string
	ChannelID string

	// Hint routes this turn explicitly — "fast", "deep". Set by a
	// channel or an API caller who already knows how much thought the
	// turn deserves. An explicit hint SKIPS the preflight call: it
	// answers the only question the preflight was going to ask.
	Hint Hint

	// IsReviewFork marks a turn as the post-turn review replaying a
	// conversation. The recursion guard: without it the first review
	// triggers the second, and a review of a review is meaningless and
	// unbounded.
	IsReviewFork bool

	// UserTimezone is the user's preferred IANA zone for time
	// rendering (e.g. "Europe/London"). Resolved from the user's
	// preferences bucket at turn assembly. Empty falls back to the
	// cluster default and finally to UTC. Threaded through to
	// builtins via the turn identity so time
	// outputs render in the user's wall-clock without the agent
	// having to remember to convert.
	UserTimezone string

	// SystemPrompt is the pre-assembled system prompt from
	// promptgen.Generate(). Callers build this once per turn (it's
	// deterministic per context) and pass it in — avoids re-building
	// across internal tool-call rounds and keeps prompt-caches warm.
	SystemPrompt string

	// Tools is the list of tool definitions the LLM may call. Built
	// from the Registry; the caller may filter by capabilities or
	// per-claim authorization before passing in.
	Tools []Tool

	// Model overrides the provider's default model. Empty → default.
	Model string

	// Budget is the per-turn spend / tool-call / egress tracker.
	// Required — no-op in practice only when caps are all zero.
	Budget *TurnBudget

	// ConversationHistory is prior user/assistant messages in this
	// conversation. Appended to before the current user turn so the
	// LLM has context. Empty for first turn.
	ConversationHistory []Message

	// ConversationSummary stands in for the part of this conversation
	// that has aged out of verbatim replay. Injected as its own
	// system message rather than folded into ConversationHistory, so
	// the context budget can't trim away the very thing that exists
	// to survive trimming.
	ConversationSummary string

	// RecalledContext is memory recall for this turn, already wrapped
	// by promptgen. Populated by fillDefaults, not by callers, and
	// delivered as a user-role message rather than in the system
	// prompt — recalled episodes are untrusted content.
	RecalledContext string

	// Attachments are media the channel received with this turn.
	// Channel handlers (gateway/telegram, gateway/rest, etc.)
	// populate this from their native payload + downloader. The
	// agent surfaces attachment metadata in the user-message turn
	// so a non-vision LLM can still reason about "the user sent
	// an image at /workspace/incoming/abc.jpg" and call MCP tools
	// (e.g. minimax.image_understanding) to actually inspect it.
	// Provider-native vision passthrough is a future addition (gated
	// on ProviderConfig.Capabilities including "vision"); today's
	// path is text-decoration + tool-driven inspection.
	Attachments []types.Attachment
}

// ProcessMessageResponse is the per-turn output.
type ProcessMessageResponse struct {
	// Reply is the assistant's final text response. Populated for
	// normal turns; empty when NeedsConfirmation is true.
	Reply string

	// ToolCalls records every tool invocation performed during the
	// turn, in order. Retained for audit and for UI that shows
	// "I ran these commands for you".
	ToolCalls []ToolInvocation

	// Attachments are files the turn PRODUCED — generated speech,
	// images, video. The channel layer sends these alongside Reply.
	//
	// They are not in Reply because they cannot be: audio is not text.
	// A tool that makes a file announces it via CollectArtifact and it
	// surfaces here, so the channel can attach the bytes rather than
	// the model reciting a file path at the user.
	Attachments []types.Attachment

	// Messages is the full conversation after this turn — the
	// caller persists this to feed subsequent turns.
	Messages []Message

	// TurnStartIndex is where this turn's own messages begin in
	// Messages; everything before it is replayed history and the
	// system prompt. Callers persisting a turn slice from here.
	//
	// The agent has to report this rather than let the caller
	// compute it: ContextBudget may drop history messages before
	// they reach Messages, so "system + len(history I passed in)"
	// is not where the turn starts.
	TurnStartIndex int

	// BudgetState is a snapshot of the TurnBudget at turn end.
	BudgetState BudgetState

	// NeedsConfirmation is true when a policy or budget check
	// requested user approval mid-turn. Channel handlers surface
	// the ConfirmationReason to the user and re-run the turn with
	// explicit approval.
	NeedsConfirmation bool

	// ConfirmationAction and ConfirmationResource name the operation
	// waiting on the user, so a channel can offer to remember the
	// answer for this conversation. Empty when the confirmation came
	// from the budget rather than a policy rule — there is no
	// operation to remember in that case, only a spend to acknowledge.
	ConfirmationAction   string
	ConfirmationResource string

	ConfirmationReason string
}

// ToolInvocation records one tool call's lifecycle within a turn.
type ToolInvocation struct {
	CallID   string
	ToolName string
	Args     string
	Output   string
	ExitCode int
	Error    string
}

// RunToolCallLoop processes one turn end-to-end. Steps per PLAN.md
// Phase 5.4:
//
//  1. Seed conversation with system prompt + history + user message.
//  2. Call LLM (via Provider).
//  3. Record usage on TurnBudget. If exceeded → NeedsConfirmation.
//  4. If response is text-only: return it. Loop done.
//  5. For each tool call in response:
//     a. TurnBudget.RecordToolCall; if exceeded → NeedsConfirmation.
//     b. Executor.Invoke (policy + hooks + sandbox inside).
//     c. Append tool-role message with ToolCallID + output.
//     d. Record egress bytes on TurnBudget.
//  6. Go to step 2 with the augmented conversation. Max MaxToolLoops.
//
// PreLLMCall / PostLLMCall hooks fire around step 2 when a
// HookDispatcher is configured.
func (a *Agent) RunToolCallLoop(ctx context.Context, req ProcessMessageRequest) (*ProcessMessageResponse, error) {
	if req.Budget == nil {
		return nil, errors.New("RunToolCallLoop: req.Budget is required")
	}
	// Attached before fillDefaults, not inside runLoop: fillDefaults is
	// where the ContextEngine runs its passive recall, and that recall
	// needs to know whose memories it may read. Getting this order wrong
	// is how the recall came to be unscoped in the first place.
	ctx = WithTurnIdentity(ctx, a.turnIdentityFor(req))
	// Attached once, at the top, so anything downstream can emit a
	// span without every intermediate signature growing a parameter.
	// A nil recorder leaves the context untouched, which is what a
	// deployment with tracing off gets.
	ctx = trace.WithTurn(ctx, a.cfg.Traces, req.TurnID)
	// Routed ONCE, at turn start. A tool-call loop dispatches many
	// times, and re-judging on each would pay for a preflight per
	// round-trip and could route one turn two different ways
	// mid-conversation.
	ctx = WithRoute(ctx, a.resolveRoute(ctx, req))
	a.fillDefaults(ctx, &req)
	seeded := a.seedMessages(req)
	// The user message is the last thing seedMessages appends, so
	// the turn starts there. When the caller sent no text (media
	// only), the turn starts at whatever the loop appends next.
	turnStart := len(seeded)
	if len(seeded) > 0 && seeded[len(seeded)-1].Role == "user" {
		turnStart = len(seeded) - 1
	}
	return a.runLoop(ctx, req, seeded, &ProcessMessageResponse{TurnStartIndex: turnStart})
}

// fillDefaults populates req.Tools from the agent's Registry and
// req.SystemPrompt from the agent's Soul when the caller left
// them empty. Channels stay transport-only — text in, reply out —
// without each having to know about tools or personality.
// Explicit values on req always win so tests that script exact
// prompts still work.
func (a *Agent) fillDefaults(ctx context.Context, req *ProcessMessageRequest) {
	if req.UserTimezone == "" && a.cfg.TimezoneResolver != nil && req.Claims != nil {
		req.UserTimezone = a.cfg.TimezoneResolver(req.Claims.UserID)
	}
	if req.Tools == nil && a.cfg.Registry != nil {
		// Send every registered tool every turn. The keyword-based
		// tailor caused recurring "I don't have that tool"
		// hallucinations whenever a category missed; at our current
		// scale (~50 tools, ~5K tokens of definitions) the token
		// cost of full advertisement is acceptable. When tool count
		// crosses ~100 we swap to semantic top-K retrieval against
		// the existing embedding service.
		req.Tools = a.cfg.Registry.LLMTools()
	}
	if req.SystemPrompt == "" && a.cfg.Soul != nil {
		soul := a.cfg.Soul()
		if soul != nil {
			var bins []promptgen.BinaryInfo
			if a.cfg.BinariesProvider != nil {
				bins = a.cfg.BinariesProvider()
			}
			var skillIndex []promptgen.SkillInfo
			if a.cfg.SkillsProvider != nil {
				skillIndex = a.cfg.SkillsProvider()
			}
			var pinned promptgen.PinnedBlocks
			if a.cfg.PinnedProvider != nil {
				pinned = a.cfg.PinnedProvider(sessionKeyFor(req), userIDFor(req))
			}
			var proposals int
			if a.cfg.ProposalsProvider != nil {
				proposals = a.cfg.ProposalsProvider(userIDFor(req))
			}
			req.SystemPrompt = promptgen.Generate(promptgen.GenerateInput{
				Soul:           soul,
				Tools:          toPromptgenTools(req.Tools),
				Skills:         skillIndex,
				SkillProposals: proposals,
				Pinned:         pinned,
				Binaries:       bins,
				Runtime: promptgen.RuntimeInfo{
					Channel:      req.Channel,
					ChannelID:    req.ChannelID,
					SelfLearning: a.cfg.SelfLearningMode,
				},
			})
		}
	}
	// Recall is carried on the request rather than folded into the
	// system prompt. Recalled episodes are untrusted — ingest stores
	// user messages verbatim, and fetched pages can be summarised into
	// memory — so the system prompt, the most privileged position in
	// the request, is the wrong place for them. seedMessages puts them
	// in a user-role message, which is the position promptgen's
	// deliberate no-escaping decision reasoned about.
	if a.cfg.ContextEngine != nil {
		assembly := a.cfg.ContextEngine.Assemble(ctx, req.Message)
		if rendered := assembly.Rendered(); rendered != "" {
			req.RecalledContext = rendered
			a.cfg.Logger.Debug("agent: context-engine recall injected",
				"turn_id", req.TurnID,
				"recall_count", len(assembly.RecallIDs))
		}
	}
}

// maybeIngestTurn fires the configured EpisodicIngester after a
// clean turn. Async by design — the reply has already been
// appended to resp before this is called, and the channel's
// response to the user is strictly downstream of that. Blocking
// on Raft + embedding would add 200-500ms to every reply for
// content the user has already received; firing in a goroutine
// removes that latency without sacrificing durability (the write
// is already eventually-consistent from the user's perspective).
//
// Context is deliberately decoupled from req.Context — the
// channel's context cancels when its handler returns (right
// after sending the reply), which would orphan our goroutine.
// Use context.Background with a bounded timeout instead.
//
// Failures log WARN and are swallowed. Memory loss on a single
// turn is preferable to dropping the user's reply for a backend
// hiccup.
func (a *Agent) maybeIngestTurn(ctx context.Context, req ProcessMessageRequest, reply string) {
	if a.cfg.EpisodicIngester == nil || reply == "" {
		return
	}
	// Channel and ChatID come from the request, which is where they
	// live. They used to be taken from Claims.Scope — the permission
	// tier — so every episodic record was tagged "channel:admin"
	// rather than "channel:telegram", and ChatID was never set at all,
	// leaving the chat tag off entirely.
	turn := EpisodicTurn{
		Channel:     req.Channel,
		ChatID:      req.ChannelID,
		UserMessage: req.Message,
		AssistReply: reply,
		TurnID:      req.TurnID,
		CompletedAt: time.Now(),
	}
	if req.Claims != nil {
		turn.UserID = req.Claims.UserID
	}
	if id, ok := TurnIdentityFrom(ctx); ok {
		turn.Owner = id.Principal.String()
	}
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.cfg.EpisodicIngester.IngestTurn(bgCtx, turn); err != nil {
			a.cfg.Logger.Warn("agent: episodic ingest failed; turn still succeeded",
				"turn_id", req.TurnID, "err", err)
		}
	}()
}

// toPromptgenTools renders the LLM-facing Tools as promptgen's
// ToolInfo shape so the system-prompt "Available Tools" section
// matches what the model actually has.
// sessionKeyFor identifies the conversation a snapshot is frozen
// against. Channel plus channel id rather than the turn id, because a
// snapshot per turn is not a snapshot.
//
// A turn with no channel — the scheduler's — gets an empty key, which
// the provider treats as its own session. That is correct: a scheduled
// turn is not part of anybody's conversation, so freezing it against
// one would hand it a stale view.
func sessionKeyFor(req *ProcessMessageRequest) string {
	return req.Channel + ":" + req.ChannelID
}

// userIDFor is whose pinned memory a turn renders. Empty claims mean
// no profile rather than somebody else's.
func userIDFor(req *ProcessMessageRequest) string {
	if req.Claims == nil {
		return ""
	}
	return req.Claims.UserID
}

func toPromptgenTools(tools []Tool) []promptgen.ToolInfo {
	out := make([]promptgen.ToolInfo, 0, len(tools))
	for _, t := range tools {
		out = append(out, promptgen.ToolInfo{
			Name:        t.Name,
			Description: t.Description,
		})
	}
	return out
}

// ResumeFromConfirmation picks up a turn that previously returned
// NeedsConfirmation. Callers must pass the prior response's Messages
// (so we re-enter mid-conversation, not from the system prompt) and a
// Budget with Relax() already called (so the step that originally
// tripped the cap can proceed). The resume may itself hit a new
// confirmation — channel handlers loop until final reply or denial.
func (a *Agent) ResumeFromConfirmation(ctx context.Context, req ProcessMessageRequest, priorMessages []Message) (*ProcessMessageResponse, error) {
	if req.Budget == nil {
		return nil, errors.New("ResumeFromConfirmation: req.Budget is required")
	}
	if len(priorMessages) == 0 {
		return nil, errors.New("ResumeFromConfirmation: priorMessages is empty — nothing to resume from")
	}
	ctx = WithTurnIdentity(ctx, a.turnIdentityFor(req))
	a.fillDefaults(ctx, &req)
	msgs := make([]Message, len(priorMessages))
	copy(msgs, priorMessages)
	// Everything handed in was already recorded when the turn
	// stopped for confirmation; only what the resumed leg appends
	// is new.
	return a.runLoop(ctx, req, msgs, &ProcessMessageResponse{TurnStartIndex: len(msgs)})
}

func (a *Agent) runLoop(ctx context.Context, req ProcessMessageRequest, messages []Message, resp *ProcessMessageResponse) (*ProcessMessageResponse, error) {
	// Every exit from this loop — normal, budget-exceeded, confirmation
	// or hard-timeout — must carry whatever files the turn produced.
	// A turn that synthesised audio and then hit its budget still
	// generated (and was billed for) the audio, so dropping it on the
	// unusual paths would lose something the user already paid for.
	ctx, artifacts := WithArtifactCollector(ctx)
	defer func() { resp.Attachments = artifacts.Collected() }()
	// Generation costs come back this way. A builtin has no reference
	// to the budget and should not: it would then be able to refuse a
	// turn on the budget's behalf, halfway through, from inside a tool.
	ctx, costs := WithCostCollector(ctx)

	// Attribution is flushed on EVERY exit — normal, budget-exceeded,
	// confirmation, hard timeout, loop exhausted. A turn that ended
	// unusually is the one whose cost somebody is asking about, so
	// buffering it behind the happy path would lose it precisely where
	// it is wanted.
	attribution := newToolAttributor(a.cfg.Traces, req.TurnID)
	defer attribution.flush()

	for loop := range a.cfg.MaxToolLoops {
		a.cfg.Logger.Debug("agent: LLM round-trip",
			"turn_id", req.TurnID, "loop", loop, "messages", len(messages))

		chatResp, err := a.callLLM(ctx, req, messages)
		if err == nil {
			attribution.noteLLMCall(chatResp.pricing)
		}
		if err != nil {
			// Context deadline / cancellation (e.g. gateway
			// hard-timeout) → produce a graceful user-visible reply
			// rather than a silent error. It needs a FRESH context,
			// because the one that just expired would cancel the
			// summary call before it started.
			//
			// Fresh but BOUNDED. context.Background() here meant a
			// provider that had stopped responding — the usual reason
			// a turn hits its hard timeout — was re-entered with a
			// context that could never cancel, so the timeout the
			// gateway set was defeated by the code meant to report it
			// gracefully and the request hung until the client gave up.
			if ctx.Err() != nil {
				summaryCtx, cancel := context.WithTimeout(
					context.WithoutCancel(ctx), a.summaryReplyTimeout())
				defer cancel()
				return a.forceSummaryReply(summaryCtx, req, messages, resp, "hard_timeout")
			}
			return nil, fmt.Errorf("LLM call: %w", err)
		}

		budgetDecision := req.Budget.RecordCostUSD(chatResp.cost)
		// Whatever the tools spent this round-trip, recorded BEFORE the
		// exceeded check below acts on the total. A video generated on
		// the round-trip that tips the turn over its cap is part of why
		// it tipped over.

		for _, rec := range costs.Drain() {
			if d := req.Budget.RecordCostUSD(rec); d.Exceeded {
				budgetDecision = d
			}
		}
		if budgetDecision.Exceeded {
			resp.NeedsConfirmation = true
			resp.ConfirmationReason = fmt.Sprintf("budget exceeded on %s", budgetDecision.ExceededOn)
			resp.BudgetState = budgetDecision.Current
			resp.Messages = messages
			return resp, nil
		}

		// Append assistant response to conversation, even if it
		// only contains tool calls — the next LLM round-trip needs
		// to see the prior tool-call request.
		assistantMsg := Message{
			Role:      "assistant",
			Content:   chatResp.Content,
			ToolCalls: chatResp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		if len(chatResp.ToolCalls) == 0 {
			// Strip reasoning-model chain-of-thought from the
			// user-facing reply. Internal messages keep the full
			// content so the next round-trip (including this
			// assistant message) still shows the model its own
			// reasoning. Only resp.Reply — what channels render
			// — gets the stripped form.
			// The chain's later steps run HERE, on the turn's
			// finished answer — not on each round-trip. Step 0 was
			// the whole tool-call loop; a reviewer applied to an
			// intermediate call would be reviewing a decision to
			// look something up. A no-op unless a multi-step chain
			// routed this turn.
			final := a.runChainSteps(ctx, req, chatResp.Content)
			resp.Reply = stripReasoningTags(final)
			resp.Messages = messages
			resp.BudgetState = req.Budget.State()
			// Fire-and-forget episodic ingest: the model
			// finished a turn, capture it for dream
			// consolidation. Nil ingester is a no-op; errors are
			// logged and swallowed because memory loss is a
			// soft failure compared to dropping the user's
			// reply.
			// The FINAL answer, not step 0's draft. What the user
			// received is what should be remembered; ingesting the
			// draft would seed memory with a reply nobody saw.
			a.maybeIngestTurn(ctx, req, final)
			a.cfg.Review.Consider(req, resp.Messages, len(resp.ToolCalls))
			return resp, nil
		}

		// Dispatch each tool call through the Executor. Results come
		// back as tool-role messages for the next LLM round-trip.
		for _, tc := range chatResp.ToolCalls {
			toolStart := time.Now()
			inv, confirmation, err := a.runToolCall(ctx, req, tc)
			if err != nil {
				return nil, fmt.Errorf("tool call %q: %w", tc.Name, err)
			}
			attribution.noteTool(inv, time.Since(toolStart), toolStart)
			resp.ToolCalls = append(resp.ToolCalls, inv)
			if confirmation != nil {
				resp.NeedsConfirmation = true
				resp.ConfirmationReason = confirmation.Reason
				resp.ConfirmationAction = confirmation.Action
				resp.ConfirmationResource = confirmation.Resource
				resp.BudgetState = req.Budget.State()
				messages = append(messages, toolResultMessage(tc, inv))
				resp.Messages = messages
				return resp, nil
			}
			messages = append(messages, toolResultMessage(tc, inv))
		}
	}

	// Loop exhausted without the LLM choosing to stop calling tools.
	// Historically this returned an error — which the gateway
	// swallowed, and the user saw nothing. Force a final summary
	// turn with tools disabled so the user always gets a reply,
	// even if it's "I tried X, Y, Z and couldn't finish."
	return a.forceSummaryReply(ctx, req, messages, resp, "tool_loop_exhausted")
}

// forceSummaryReply makes one last LLM call with tools stripped,
// asking the model to wrap up honestly. Called when the agent loop
// would otherwise return an error without a user-visible reply
// (loop exhausted, hard timeout, etc.). Returns the reply in the
// same shape as a successful turn.
// defaultSummaryReplyTimeout bounds the graceful "I ran out of time"
// reply when nothing else says. Short on purpose: the user has already
// waited out the whole turn budget, and a summary that takes as long
// again is worse than a blunter error arriving now.
const defaultSummaryReplyTimeout = 15 * time.Second

// summaryReplyTimeout resolves the configured bound.
func (a *Agent) summaryReplyTimeout() time.Duration {
	if a.cfg.SummaryTimeout > 0 {
		return a.cfg.SummaryTimeout
	}
	return defaultSummaryReplyTimeout
}

// staticFallbackReply is what the user sees when even the graceful
// summary call fails — the provider is not answering at all.
func staticFallbackReply(reason string) string {
	switch reason {
	case "hard_timeout":
		return "This took too long and I had to stop. Nothing I did is lost — ask again and I'll pick it up."
	default:
		return "I hit my tool-call limit for this turn and couldn't complete the task. Try rephrasing or narrowing the request."
	}
}

func (a *Agent) forceSummaryReply(
	ctx context.Context,
	req ProcessMessageRequest,
	messages []Message,
	resp *ProcessMessageResponse,
	reason string,
) (*ProcessMessageResponse, error) {
	var instruction string
	switch reason {
	case "tool_loop_exhausted":
		instruction = "You have reached the maximum number of tool calls for this turn. Do NOT call any more tools. Reply to the user in plain text: explain what you were trying to do, what you learned from the tools you did run, and what you couldn't complete. Be honest and concise."
	case "hard_timeout":
		instruction = "This turn has run too long. Do NOT call any more tools. Reply to the user in plain text: summarise progress so far, what succeeded, and what remains unfinished. Be concise."
	default:
		instruction = "Reply to the user in plain text without calling any more tools."
	}

	// MiniMax + several other providers reject role=system anywhere
	// except position 0 (HTTP 400 "invalid message role: system").
	// Use a user-role nudge instead — it's universally accepted and
	// the model treats it the same operationally (final-turn
	// instruction directing the next response).
	forced := make([]Message, 0, len(messages)+1)
	forced = append(forced, messages...)
	forced = append(forced, Message{Role: "user", Content: instruction})

	// Build a ChatRequest with tools explicitly stripped so the
	// model cannot emit another tool-call even if it wanted to.
	forcedReq := req
	forcedReq.Tools = nil

	chatResp, err := a.callLLM(ctx, forcedReq, forced)
	if err != nil {
		// Can't even get a summary out — fall back to a static
		// apology so the user sees SOMETHING rather than silence.
		a.cfg.Logger.Warn("agent: forced-summary LLM call failed; returning static fallback",
			"turn_id", req.TurnID, "reason", reason, "err", err)
		// Wording follows the reason. Telling somebody whose turn ran
		// out of time that it hit a tool-call limit sends them off to
		// narrow a request that was never too broad.
		resp.Reply = staticFallbackReply(reason)
		resp.Messages = messages
		if req.Budget != nil {
			resp.BudgetState = req.Budget.State()
		}
		return resp, nil
	}

	if req.Budget != nil {
		req.Budget.RecordCostUSD(chatResp.cost)
		resp.BudgetState = req.Budget.State()
	}
	resp.Reply = stripReasoningTags(chatResp.Content)
	messages = append(messages, Message{Role: "assistant", Content: chatResp.Content})
	resp.Messages = messages
	a.maybeIngestTurn(ctx, req, chatResp.Content)
	// After the reply is assembled, never before. A learning side
	// effect must not delay somebody's answer, and Consider does not
	// block.
	a.cfg.Review.Consider(req, messages, len(resp.ToolCalls))
	return resp, nil
}

// seedMessages builds the initial message list from the system
// prompt + conversation history + the user's current message.
// When the channel delivered attachments alongside the message, we
// decorate the user-turn with a structured note so the LLM can
// reason about + reference them via tool calls (e.g. open the image
// with minimax.image_understanding, transcribe the voice note with
// a future STT MCP). The decoration is deterministic across turns
// so prompt caches stay warm.
func (a *Agent) seedMessages(req ProcessMessageRequest) []Message {
	// Budget the replayed history before it reaches the wire. Done
	// here rather than at the channel so scheduler-originated turns
	// and every future channel inherit the same policy.
	history := a.cfg.ContextBudget.Apply(req.ConversationHistory)
	if dropped := len(req.ConversationHistory) - len(history); dropped > 0 {
		a.cfg.Logger.Debug("agent: history trimmed to context budget",
			"turn_id", req.TurnID,
			"dropped_messages", dropped,
			"kept_messages", len(history),
			"budget", a.cfg.ContextBudget.String())
	}
	out := make([]Message, 0, len(history)+3)
	if req.SystemPrompt != "" {
		out = append(out, Message{Role: "system", Content: req.SystemPrompt})
	}
	if s := strings.TrimSpace(req.ConversationSummary); s != "" {
		out = append(out, Message{
			Role: "system",
			Content: "Summary of earlier parts of this conversation, which are no longer shown in full:\n\n" + s +
				"\n\nTreat this as your own recollection. Do not mention the summary to the user.",
		})
	}
	out = append(out, history...)

	// Recall sits immediately before the user's message rather than at
	// the head of the list. R5 calls it a "leading" context message and
	// gives prompt-prefix caching as one motivation; placing it here is
	// what delivers that, because everything above stays byte-identical
	// between turns while recall changes every turn.
	if r := strings.TrimSpace(req.RecalledContext); r != "" {
		out = append(out, Message{Role: "user", Content: r})
	}

	userText := decorateWithAttachments(req.Message, req.Attachments)
	if userText != "" {
		out = append(out, Message{Role: "user", Content: userText})
	}

	// A turn has to end on something the model can answer. With an
	// empty message, no attachments and no history, everything above
	// this point is system-role — so the last thing in the request is
	// the system prompt, and the model replies to THAT: it agrees to
	// its own instructions and describes its own configuration.
	//
	// Every channel in tree already guards this at its own door (REST
	// rejects the request, Telegram and Slack substitute a stub for a
	// caption-less upload). This is the backstop for the callers that
	// are not channels — research workers, and whatever drives the
	// agent next — because the failure is silent, reaches the user as
	// a reply, and looks like a model defect rather than a malformed
	// request.
	if !hasAddressableTurn(out) {
		a.cfg.Logger.Warn("agent: turn has no user content; the model would be answering its own system prompt",
			"turn_id", req.TurnID, "channel", req.Channel)
		out = append(out, Message{
			Role:    "user",
			Content: "(no message content was received — say briefly that nothing came through, and ask what they need)",
		})
	}
	return out
}

// hasAddressableTurn reports whether the request ends on something
// other than system-role scaffolding. Checks the LAST message rather
// than "is there a user message anywhere": a transcript replayed after
// a tool call legitimately ends on a tool result, and that is a turn
// the model can continue.
func hasAddressableTurn(msgs []Message) bool {
	if len(msgs) == 0 {
		return false
	}
	return msgs[len(msgs)-1].Role != "system"
}

// decorateWithAttachments appends an "[attached: ...]" block to the
// user's text so a text-only LLM can still reason about the media
// and pick the right tool to inspect it. When there are no
// attachments, returns text unchanged.
//
// The hint after the attachment list nudges the agent toward the
// right action: when read_image is registered, calling it on the
// LocalPath is the only way for a text-only main model to actually
// see an image. Without this, the model would reply "I can't view
// images" — accurate but unhelpful when a vision tool is sitting
// right there in its tool list.
func decorateWithAttachments(text string, attachments []types.Attachment) string {
	if len(attachments) == 0 {
		return text
	}
	var b strings.Builder
	if text != "" {
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	b.WriteString("[user attached ")
	for i, a := range attachments {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.Describe())
	}
	b.WriteString("]")

	var hasImage, hasAudio, hasPDF bool
	for _, a := range attachments {
		switch {
		case a.Kind == types.AttachmentImage, a.Kind == types.AttachmentSticker, strings.HasPrefix(a.MimeType, "image/"):
			hasImage = true
		case a.Kind == types.AttachmentVoice, a.Kind == types.AttachmentAudio,
			strings.HasPrefix(a.MimeType, "audio/"):
			hasAudio = true
		case strings.HasSuffix(strings.ToLower(a.Filename), ".pdf"),
			a.MimeType == "application/pdf":
			hasPDF = true
		}
	}
	if hasImage {
		b.WriteString("\n[hint: call read_image(path=...) on the path above to view the image. If read_image is not in your tool list, no vision tool is wired — say so plainly rather than pretending to see it.]")
	}
	if hasAudio {
		b.WriteString("\n[hint: call read_audio(path=...) on the path above to transcribe the audio. If read_audio is not in your tool list, no audio tool is wired — say so plainly rather than pretending to hear it.]")
	}
	if hasPDF {
		b.WriteString("\n[hint: call read_pdf(path=...) on the path above to read the document. If read_pdf is not in your tool list, no PDF tool is wired — say so plainly rather than pretending to read it.]")
	}
	return b.String()
}

// chatWithCost wraps ChatResponse with the CostRecord computed
// for it. Agent.callLLM returns one so the caller can record spend
// against the budget with full attribution in one place.
type chatWithCost struct {
	*ChatResponse
	cost CostRecord
	// pricing is the winning provider's rate card, carried so the
	// context-carry attribution can price re-sent tokens at the same
	// rate the turn was actually billed at.
	pricing types.ProviderPricing
}

// callLLM dispatches the LLM round-trip, fires PreLLMCall /
// PostLLMCall hooks around it, and packages the usage with a cost
// record. The caller records spend via TurnBudget.RecordCostUSD.
func (a *Agent) callLLM(ctx context.Context, req ProcessMessageRequest, messages []Message) (*chatWithCost, error) {
	if a.cfg.Hooks != nil {
		_, err := a.cfg.Hooks.Dispatch(ctx, types.HookPreLLMCall, map[string]any{
			"turn_id": req.TurnID,
			"scope":   scopeOfClaims(req.Claims),
		})
		if err != nil {
			// Hook blocked — propagate as-is so the caller sees
			// ErrHookBlocked.
			return nil, err
		}
	}

	chatReq := ChatRequest{
		Messages: messages,
		Model:    req.Model,
		Tools:    req.Tools,
	}
	dispatched, err := a.dispatchWithBackup(ctx, chatReq)
	if err != nil {
		return nil, err
	}
	chatResp := dispatched.resp

	// The cost of a turn is a function of the provider that served it,
	// and until dispatchWithBackup returned the winning entry the
	// caller had no way to know which one that was — so this was built
	// with an empty label and a hardcoded zero. Every turn to date has
	// reported a spend of nothing, and the budget's spend cap has
	// therefore never fired.
	//
	// The model comes from the entry rather than from req.Model,
	// because a failover means the reply came from a different model
	// than the one asked for, and attributing the cost to the requested
	// one would misprice exactly the turns worth auditing.
	model := dispatched.entry.Model
	if model == "" {
		model = req.Model
	}
	cost := RecordCost(dispatched.entry.Label, model, chatResp.Usage, dispatched.entry.Pricing)

	if a.cfg.Hooks != nil {
		// Hooks are best-effort observability; their own pipeline
		// surfaces failures via its log. Bubbling here would let a
		// broken hook script block the turn — which is the wrong
		// failure mode for "I want to count tokens."
		_, _ = a.cfg.Hooks.Dispatch(ctx, types.HookPostLLMCall, map[string]any{ //nolint:errcheck // see comment above
			"turn_id":    req.TurnID,
			"scope":      scopeOfClaims(req.Claims),
			"usage":      chatResp.Usage,
			"finish":     chatResp.FinishReason,
			"tool_calls": len(chatResp.ToolCalls),
		})
	}

	return &chatWithCost{ChatResponse: chatResp, cost: cost, pricing: dispatched.entry.Pricing}, nil
}

// dispatchWithBackup calls the primary LLM provider; on a hard
// failure (rate-limit, 5xx, timeout, network refused) walks the
// ProviderRegistry backup chain and retries on each subsequent
// provider. Same-turn transparent fallback — the user sees one
// reply from whichever provider succeeds.
//
// Soft errors (context cancellation, 4xx other than 429) bubble
// immediately; they're not indicators of provider failure and
// retrying wouldn't help.
// errText renders an error for a log attribute without panicking on
// nil — the backup-succeeded line fires when the only prior events
// were skips.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// bareDispatch is the no-registry path: one injected provider, no
// chain, no label to attribute a cost to.
func (a *Agent) bareDispatch(ctx context.Context, req ChatRequest) (*dispatchResult, error) {
	resp, err := a.cfg.Provider.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	return &dispatchResult{resp: resp}, nil
}

// attemptSpan records one provider attempt.
//
// Every attempt, not just the winning one. "My primary is never used"
// and "the chain failed over twice before succeeding" are the two
// questions failover makes unanswerable, and both need the losers.
//
// Carries no content: a provider label, a model name, a duration, token
// counts and a classified error. The error text comes from the driver's
// own classification rather than a response body, because a body is
// where a provider echoes the prompt back.
func attemptSpan(turnID string, entry ProviderEntry, elapsed time.Duration, started time.Time,
	attempt int, outcome trace.Outcome, resp *ChatResponse, err error) trace.Span {
	span := trace.Span{
		TurnID:    turnID,
		SpanID:    ids.New(),
		Kind:      trace.KindLLMCall,
		Name:      entry.Model,
		Provider:  entry.Label,
		StartedAt: started,
		Duration:  elapsed,
		Outcome:   outcome,
		Attempt:   attempt,
	}
	if err != nil {
		span.Error = err.Error()
	}
	if resp != nil {
		span.Usage = trace.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			CachedTokens:     resp.Usage.CachedTokens,
		}
		span.CostUSD = EstimateCost(resp.Usage, entry.Pricing)
	}
	return span
}

// reportBelowFloor names a provider the floor excluded, once per
// label per process.
//
// Once, because this fires on every turn for as long as the config
// stays as it is, and a line repeated per turn is one an operator
// filters out — including the first time it would have told them
// something. Warn rather than debug: a provider silently dropped from
// a failover chain is the difference between a resilient deployment
// and one that looks resilient.
func (a *Agent) reportBelowFloor(label string, tier, floor types.TrustTier) {
	if _, seen := a.belowFloorReported.LoadOrStore(label, struct{}{}); seen {
		return
	}
	a.cfg.Logger.Warn("agent: provider excluded by the trust floor",
		"label", label, "trust_tier", tier, "min_trust_tier", floor)
}

// dispatchResult is the response plus WHICH provider produced it.
//
// The winning entry has to come back, because the cost of a turn is a
// function of the provider that served it and the caller had no way to
// know which one that was. That is why CostRecord has been built with
// an empty label and a zero cost since it was written.
type dispatchResult struct {
	resp  *ChatResponse
	entry ProviderEntry
}

func (a *Agent) dispatchWithBackup(ctx context.Context, req ChatRequest) (*dispatchResult, error) {
	// Single-provider mode: no registry wired, no chain to walk. The
	// floor cannot be enforced here — there is no tier to read, only a
	// bare LLMProvider — so boot-time validation is what covers this
	// path, and it refuses to start rather than letting a turn run
	// unchecked.
	if a.cfg.Providers == nil || a.cfg.PrimaryLabel == "" {
		return a.bareDispatch(ctx, req)
	}

	// The chain decides where the walk BEGINS; the walk itself is
	// unchanged, so the trust floor, health cooldowns and per-attempt
	// spans all still apply to every candidate after the first.
	start := a.cfg.PrimaryLabel
	if route := RouteFrom(ctx); route != nil && route.StartLabel != "" {
		start = route.StartLabel
	}
	chain := a.cfg.Providers.Chain(start)
	if len(chain) == 0 {
		return a.bareDispatch(ctx, req)
	}
	rec, turnID := trace.FromContext(ctx)

	// Read once per dispatch rather than once per candidate, so a
	// soul tuned mid-chain cannot let one turn use two different
	// floors.
	floor := FloorOf(a.cfg.Soul)

	var (
		lastErr    error
		skipped    int
		considered []TrustCandidate
		belowFloor int
		attempt    int
	)
	for _, entry := range chain {
		// The floor, at EVERY candidate.
		//
		// It was checked nowhere on this path: the only code that read
		// min_trust_tier was the chain Resolver, which nothing calls.
		// So an operator could set a floor, watch it render into the
		// prompt, and have a turn silently complete on a public
		// provider the moment the primary returned a 429 — the
		// failover machinery that makes the assistant resilient was
		// the same machinery that lowered the floor.
		considered = append(considered, TrustCandidate{Label: entry.Label, Tier: entry.TrustTier})
		if !MeetsFloor(floor, entry.TrustTier) {
			belowFloor++
			a.reportBelowFloor(entry.Label, entry.TrustTier, floor)
			rec.Record(trace.SkippedSpan(turnID, ids.New(), entry.Label,
				"below the trust floor", attempt))
			attempt++
			continue
		}
		// A provider that failed recently is skipped rather than
		// re-tried. Without this the chain pays a round-trip and a
		// timeout on every turn to rediscover a key that was revoked
		// this morning.
		if !a.cfg.Health.Available(entry.Label) {
			skipped++
			a.cfg.Logger.Debug("agent: skipping demoted provider",
				"label", entry.Label,
				"cooldown_remaining", a.cfg.Health.CooldownRemaining(entry.Label))
			rec.Record(trace.SkippedSpan(turnID, ids.New(), entry.Label,
				"in cooldown", attempt))
			attempt++
			continue
		}
		started := time.Now()
		resp, err := entry.Client.Chat(ctx, req)
		elapsed := time.Since(started)
		if err == nil {
			a.cfg.Health.RecordSuccess(entry.Label)
			rec.Record(attemptSpan(turnID, entry, elapsed, started, attempt,
				trace.OutcomeOK, resp, nil))
			if lastErr != nil || skipped > 0 {
				a.cfg.Logger.Info("agent: provider backup succeeded",
					"used_label", entry.Label,
					"skipped_demoted", skipped,
					"prior_error", errText(lastErr))
			}
			return &dispatchResult{resp: resp, entry: entry}, nil
		}
		if !isRetryableProviderError(ctx, err) {
			rec.Record(attemptSpan(turnID, entry, elapsed, started, attempt,
				trace.OutcomeAborted, nil, err))
			return nil, err
		}
		a.cfg.Health.RecordFailure(entry.Label, ClassifyFailure(err))
		logProviderFailure(a.cfg.Logger, err, "failed_label", entry.Label)
		rec.Record(attemptSpan(turnID, entry, elapsed, started, attempt,
			trace.OutcomeAdvanced, nil, err))
		attempt++
		lastErr = err
	}
	// The floor beat the chain, and that is not an outage. Reported as
	// its own error because waiting does not fix it and an operator
	// sent to the logs looking for a provider problem would find a
	// healthy one.
	if lastErr == nil && belowFloor > 0 && belowFloor+skipped == len(chain) {
		return nil, &ErrBelowTrustFloor{Floor: floor, Considered: considered}
	}
	if lastErr == nil {
		// Every provider was skipped as demoted and none was actually
		// tried. Reported distinctly: "all providers failed" with no
		// error to show would read as a bug in the chain rather than
		// as the chain protecting itself.
		return nil, fmt.Errorf(
			"agent: every provider in the chain is in cooldown (%d demoted); "+
				"check the logs for credential or quota errors", skipped)
	}
	return nil, fmt.Errorf("agent: all providers in chain failed; last error: %w", lastErr)
}

// isRetryableProviderError decides whether to walk to the next
// provider in the chain. Context-cancelled errors are NOT retryable —
// the user intent has changed or the hard-timeout fired, and retrying
// on a backup inside the same cancelled context spends the backup's
// quota on a turn nobody is waiting for.
//
// A driver that classifies its failure decides this structurally.
// Only an UNCLASSIFIED error falls through to the text scan below,
// which predates the driver waist: it reads the error message looking
// for "429", "500" and friends, so a correctly-classified transient
// failure whose text happens not to contain a magic number would
// otherwise never fail over. The scan stays for providers that do not
// wrap their errors yet — removing it would turn their transient
// failures permanent — but classified errors must never reach it.
func isRetryableProviderError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}

	var de *DriverError
	if errors.As(err, &de) {
		switch de.Class {
		case FailureTransient:
			return true
		case FailureQuotaExhausted:
			// This provider is spent; the next one has its own budget.
			// Treating a spent plan as permanent turns "one provider ran
			// out of credit" into "the assistant is down".
			return true
		case FailureCredential:
			// The next provider authenticates with its own key. Treating
			// a rejected credential as permanent turns "one key expired"
			// into "the assistant is down" — with two working providers
			// configured and idle.
			return true
		default: // FailurePermanent
			// Fails identically on the backup, so walking the chain
			// multiplies one error into one per provider.
			return false
		}
	}

	msg := strings.ToLower(err.Error())
	// Hard transient signals: rate limits, 5xx, connection
	// refused, i/o timeout. Matching on substring is crude but
	// sufficient — the LLMClient formats these consistently.
	for _, sig := range []string{
		"rate limit", "429", "500", "502", "503", "504",
		"connection refused", "timeout", "deadline",
		"minimax status 1002", // MiniMax RPM limit
		// OpenRouter returns 404 with "no endpoints available
		// matching your guardrail restrictions and data policy"
		// when the user's privacy settings exclude the providers
		// that serve this model. Semantically equivalent to 503 —
		// THIS endpoint can't serve THIS model right now; the
		// backup chain may have a model with compliant providers.
		"no endpoints available", "data policy",
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// turnIdentityFor derives the caller identity from the request.
// Channel + ChannelID are the conversation the user is already in;
// scheduler and research turns leave them empty and fall back to pure
// ownership, which is right — they carry the claims of the person the
// work is being done for (see Node.schedulerClaims), not of a chat.
func (a *Agent) turnIdentityFor(req ProcessMessageRequest) TurnIdentity {
	t := TurnIdentity{
		TurnID:    req.TurnID,
		Channel:   req.Channel,
		ChannelID: req.ChannelID,
		Timezone:  req.UserTimezone,
	}
	if req.Claims != nil {
		t.UserID = req.Claims.UserID
		t.Scope = req.Claims.Scope
		t.Roles = req.Claims.Roles
	}
	// A nil resolver maps every id to itself, which is the correct
	// behaviour for a deployment that has declared no aliases.
	t.Principal = a.cfg.Identity.Resolve(t.UserID)
	return t
}

// syntheticArgPrefix marks tool arguments the agent injects rather
// than the model supplying. Reserved: anything arriving from the model
// under this prefix is discarded.
const syntheticArgPrefix = "__"

// runToolCall dispatches one tool call through the Executor.
// Executor internally handles policy evaluation, PreToolUse +
// PostToolUse hooks, and the sandbox. On budget exceed, returns a
// non-empty confirmation string; on executor errors that SHOULD
// surface to the model as tool output (non-zero exit, for instance),
// packages them into the ToolInvocation rather than returning an
// error.
// pendingConfirmation is what a tool call needs a human for. Action
// and Resource are set only when a policy rule asked — a budget
// confirmation is about spend, not about an operation, so there is
// nothing a channel could sensibly offer to remember.
type pendingConfirmation struct {
	Reason   string
	Action   string
	Resource string
}

func (a *Agent) runToolCall(ctx context.Context, req ProcessMessageRequest, tc ToolCall) (ToolInvocation, *pendingConfirmation, error) {
	budgetDec := req.Budget.RecordToolCall()
	if budgetDec.Exceeded {
		return ToolInvocation{
			CallID:   tc.ID,
			ToolName: tc.Name,
			Args:     tc.Arguments,
			Error:    "budget exceeded",
		}, &pendingConfirmation{Reason: fmt.Sprintf("budget exceeded on %s", budgetDec.ExceededOn)}, nil
	}

	params, err := parseToolArgs(tc.Arguments)
	// Inject synthetic args from the request context so builtins
	// like commitment_create can capture the originating channel
	// without the LLM having to reliably remember to pass them.
	// Names are __-prefixed to avoid colliding with bot-provided
	// args. Builtins that care look up the prefixed key; others
	// ignore it (Go map access is forgiving). Done even on parse
	// errors below since the resulting empty params still
	// benefits from the synthetic context.
	if params == nil {
		params = make(map[string]string)
	}
	// Identity is NOT passed here. It travels on the context as a
	// TurnIdentity, because this map is built from the model's own JSON
	// and anything placed in it is a value the model can also supply.
	// The synthetic "__" keys that used to carry it are stripped rather
	// than trusted — nothing reads them any more, and a leftover one
	// arriving from the model should never reach a builtin that a later
	// change teaches to look.
	for k := range params {
		if strings.HasPrefix(k, syntheticArgPrefix) {
			delete(params, k)
		}
	}
	if err != nil {
		return ToolInvocation{
			CallID:   tc.ID,
			ToolName: tc.Name,
			Args:     tc.Arguments,
			Error:    fmt.Sprintf("parse args: %v", err),
		}, nil, nil
	}

	inv := ToolInvocation{
		CallID:   tc.ID,
		ToolName: tc.Name,
		Args:     tc.Arguments,
	}

	// Skill dispatch takes precedence when the name matches a
	// registered skill. Keeps the executor unaware of skills and
	// lets skill-level errors surface to the model distinctly.
	//
	// Policy gate: skills + MCP tools go through the same
	// tool:exec policy as builtins. Without this, the skill path
	// silently bypassed every operator allow/deny rule. The
	// executor's CheckPolicy is the same function builtin
	// dispatch uses internally, so allow rules behave
	// identically across all dispatch paths.
	if a.cfg.Skills != nil && a.cfg.Skills.Has(tc.Name) {
		if a.cfg.Executor != nil {
			if err := a.cfg.Executor.CheckPolicy(ctx, req.Claims, "tool:exec", tc.Name); err != nil {
				inv.Error = err.Error()
				if errors.Is(err, ErrRequireConfirm) {
					return inv, &pendingConfirmation{
						Reason: confirmationReason(err), Action: "tool:exec", Resource: tc.Name,
					}, nil
				}
				return inv, nil, nil
			}
		}
		skillParams := make(map[string]any, len(params))
		for k, v := range params {
			skillParams[k] = v
		}
		skillRes, err := a.cfg.Skills.Invoke(ctx, SkillInvokeRequest{
			Name:   tc.Name,
			Params: skillParams,
			Claims: req.Claims,
			TurnID: req.TurnID,
		})
		if err != nil {
			inv.Error = err.Error()
			return inv, nil, nil
		}
		inv.ExitCode = skillRes.ExitCode
		inv.Output = combineSkillOutputs(skillRes)
		req.Budget.RecordEgressBytes(int64(len(skillRes.Stdout) + len(skillRes.Stderr)))
		return inv, nil, nil
	}

	if a.cfg.Executor == nil {
		inv.Error = fmt.Sprintf("tool %q not found (no executor or skill dispatcher registered)", tc.Name)
		return inv, nil, nil
	}
	invReq := InvokeRequest{
		ToolName: tc.Name,
		Params:   params,
		Claims:   req.Claims,
		TurnID:   req.TurnID,
	}
	result, err := a.cfg.Executor.Invoke(ctx, invReq)
	if err != nil {
		// A policy rule asking for confirmation must reach the USER.
		// Returning it as a tool error hands it to the model instead,
		// which turns require_confirmation into a deny with a
		// confusing message — the operator writes a rule expecting to
		// be asked, and is never asked. docs/architecture/agent-loop
		// has always described the intended behaviour ("pause turn and
		// ask the channel; resume when user replies"); only the budget
		// path implemented it.
		if errors.Is(err, ErrRequireConfirm) {
			inv.Error = err.Error()
			return inv, &pendingConfirmation{
				Reason: confirmationReason(err), Action: "tool:exec", Resource: tc.Name,
			}, nil
		}
		inv.Error = err.Error()
		return inv, nil, nil
	}

	inv.ExitCode = result.ExitCode
	inv.Output = combineOutputs(result)

	req.Budget.RecordEgressBytes(int64(len(result.Stdout) + len(result.Stderr)))

	return inv, nil, nil
}

// combineSkillOutputs formats a skill result the same way
// combineOutputs formats an executor result — stdout first, then
// "---stderr---" delimiter + stderr on non-success. Keeps the
// model's view of skill vs tool output homogeneous.
func combineSkillOutputs(r *SkillInvokeResult) string {
	out := string(r.Stdout)
	if len(r.Stderr) > 0 && r.ExitCode != 0 {
		if len(out) > 0 {
			out += "\n---stderr---\n"
		}
		out += string(r.Stderr)
	}
	return out
}

// parseToolArgs turns the JSON-encoded args string from the LLM's
// tool call into the map[string]string the Executor's InvokeRequest
// expects. Arg values that aren't strings are stringified — the
// Executor's argv template substitutes strings only.
func parseToolArgs(arguments string) (map[string]string, error) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" || arguments == "{}" {
		return map[string]string{}, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(arguments), &raw); err != nil {
		return nil, fmt.Errorf("unmarshal tool arguments: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch vv := v.(type) {
		case string:
			out[k] = vv
		case bool, float64, int, int64:
			out[k] = fmt.Sprint(vv)
		default:
			// Complex types (nested object, array) → JSON-encode so
			// the tool sees something representable.
			if b, err := json.Marshal(v); err == nil {
				out[k] = string(b)
			}
		}
	}
	return out, nil
}

// combineOutputs merges stdout+stderr for the tool-role message
// the next LLM round-trip sees. Stderr is included because many
// tools (compilers, linters) write meaningful diagnostics there.
// Marker prefixes make it possible for the model to tell them apart.
func combineOutputs(r *InvokeResult) string {
	var b strings.Builder
	if len(r.Stdout) > 0 {
		b.WriteString("[stdout]\n")
		b.Write(r.Stdout)
		if !endsWithNewline(r.Stdout) {
			b.WriteByte('\n')
		}
	}
	if len(r.Stderr) > 0 {
		b.WriteString("[stderr]\n")
		b.Write(r.Stderr)
		if !endsWithNewline(r.Stderr) {
			b.WriteByte('\n')
		}
	}
	if r.Truncated {
		b.WriteString("[output truncated — exceeded MaxOutputBytes]\n")
	}
	return b.String()
}

// endsWithNewline is a tiny helper used by combineOutputs.
func endsWithNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}

// toolResultMessage builds the tool-role message fed back into the
// LLM conversation. The ToolCallID correlates with the originating
// assistant tool-call so the model can match outputs to requests.
// Content is wrapped in trust delimiters so the model treats tool
// output as untrusted data, not instructions.
func toolResultMessage(tc ToolCall, inv ToolInvocation) Message {
	var content string
	// Redacted on the way in. A failing command routinely echoes the
	// argument that failed, and that argument is sometimes the key —
	// which would then sit in the transcript, get replayed every turn,
	// and be summarised into memory. Replacing the secret keeps the
	// error readable, which truncating the message would not.
	if inv.Error != "" {
		content = promptgen.WrapContext([]promptgen.ContextBlock{{
			Source:  "tool:" + tc.Name + ":error",
			Trust:   promptgen.TrustUntrusted,
			Content: promptguard.Redact(inv.Error),
		}})
	} else {
		content = promptgen.WrapContext([]promptgen.ContextBlock{{
			Source:  "tool:" + tc.Name + ":output",
			Trust:   promptgen.TrustUntrusted,
			Content: promptguard.Redact(inv.Output),
		}})
	}
	return Message{
		Role:       "tool",
		Content:    content,
		ToolCallID: tc.ID,
	}
}

// scopeOfClaims extracts the Scope for hook payloads, returning ""
// for nil claims so hooks don't receive a missing field.
func scopeOfClaims(c *types.Claims) string {
	if c == nil {
		return ""
	}
	return c.Scope
}

// confirmationReason renders a require-confirmation error as the text
// a channel shows the user. The sentinel prefix is stripped: "tool
// invocation requires confirmation: rule \"x\" matched" tells the
// operator nothing they did not already know, and the part after the
// colon is the rule's own reason, which is what they wrote it for.
func confirmationReason(err error) string {
	msg := err.Error()
	if _, rest, found := strings.Cut(msg, ErrRequireConfirm.Error()+": "); found {
		return rest
	}
	return msg
}
