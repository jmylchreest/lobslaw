package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

	// Executor runs tool invocations. Required for any turn that
	// involves tool calls (i.e. most of them).
	Executor *Executor

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

	// PrimaryLabel names the provider that maps to Provider above
	// in the registry. Used as the starting point for backup-chain
	// walks. Empty → no chain walk, single-provider behaviour.
	PrimaryLabel string

	// Providers supplies label-keyed lookup for backup-chain
	// fallback + council tools. nil → single-provider mode
	// (Provider is the only LLM path).
	Providers *ProviderRegistry

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

	// Skills routes tool-call names that match a registered skill
	// through the skill invoker instead of the tool Executor. Nil
	// disables skill dispatch — every tool-call goes through the
	// executor. The interface is narrow on purpose: the agent
	// shouldn't know what a manifest is.
	Skills SkillDispatcher

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
	Channel     string
	ChatID      string
	UserID      string
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
}

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
	// address proactive replies (notify_telegram) back to the
	// correct chat when storing prompts in commitments / scheduled
	// tasks. Empty for internally-originated turns (scheduler).
	Channel   string
	ChannelID string

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

	// Messages is the full conversation after this turn — the
	// caller persists this to feed subsequent turns.
	Messages []Message

	// BudgetState is a snapshot of the TurnBudget at turn end.
	BudgetState BudgetState

	// NeedsConfirmation is true when a policy or budget check
	// requested user approval mid-turn. Channel handlers surface
	// the ConfirmationReason to the user and re-run the turn with
	// explicit approval.
	NeedsConfirmation  bool
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
	a.fillDefaults(ctx, &req)
	return a.runLoop(ctx, req, a.seedMessages(req), &ProcessMessageResponse{})
}

// fillDefaults populates req.Tools from the agent's Registry and
// req.SystemPrompt from the agent's Soul when the caller left
// them empty. Channels stay transport-only — text in, reply out —
// without each having to know about tools or personality.
// Explicit values on req always win so tests that script exact
// prompts still work.
func (a *Agent) fillDefaults(ctx context.Context, req *ProcessMessageRequest) {
	if req.Tools == nil && a.cfg.Registry != nil {
		all := a.cfg.Registry.LLMTools()
		// Heuristic tool filter: keep defaults + category
		// matches. Trims prompt size by ~40-60% on typical
		// chat turns without sacrificing capability — the
		// model still has memory_search and current_time for
		// any intent class the classifier missed.
		req.Tools = tailoredToolsFor(req.Message, all)
		if len(req.Tools) < len(all) {
			a.cfg.Logger.Debug("agent: tool list tailored",
				"turn_id", req.TurnID,
				"full", len(all),
				"tailored", len(req.Tools))
		}
	}
	if req.SystemPrompt == "" && a.cfg.Soul != nil {
		soul := a.cfg.Soul()
		if soul != nil {
			req.SystemPrompt = promptgen.Generate(promptgen.GenerateInput{
				Soul:  soul,
				Tools: toPromptgenTools(req.Tools),
				Runtime: promptgen.RuntimeInfo{
					Channel:   req.Channel,
					ChannelID: req.ChannelID,
				},
			})
		}
	}
	// Context engine runs after the base prompt is assembled —
	// its addition is appended, not prepended. Recall is purely
	// additive; the model still gets identity + operating
	// principles at the top.
	if a.cfg.ContextEngine != nil {
		assembly := a.cfg.ContextEngine.Assemble(ctx, req.Message)
		if assembly.SystemPromptAddition != "" {
			req.SystemPrompt += assembly.SystemPromptAddition
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
func (a *Agent) maybeIngestTurn(_ context.Context, req ProcessMessageRequest, reply string) {
	if a.cfg.EpisodicIngester == nil || reply == "" {
		return
	}
	turn := EpisodicTurn{
		UserMessage: req.Message,
		AssistReply: reply,
		TurnID:      req.TurnID,
		CompletedAt: time.Now(),
	}
	if req.Claims != nil {
		turn.UserID = req.Claims.UserID
		turn.Channel = req.Claims.Scope
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
	a.fillDefaults(ctx, &req)
	msgs := make([]Message, len(priorMessages))
	copy(msgs, priorMessages)
	return a.runLoop(ctx, req, msgs, &ProcessMessageResponse{})
}

func (a *Agent) runLoop(ctx context.Context, req ProcessMessageRequest, messages []Message, resp *ProcessMessageResponse) (*ProcessMessageResponse, error) {
	for loop := range a.cfg.MaxToolLoops {
		a.cfg.Logger.Debug("agent: LLM round-trip",
			"turn_id", req.TurnID, "loop", loop, "messages", len(messages))

		chatResp, err := a.callLLM(ctx, req, messages)
		if err != nil {
			// Context deadline / cancellation (e.g. gateway
			// hard-timeout) → produce a graceful user-visible
			// reply via a FRESH context rather than a silent error.
			if ctx.Err() != nil {
				return a.forceSummaryReply(context.Background(), req, messages, resp, "hard_timeout")
			}
			return nil, fmt.Errorf("LLM call: %w", err)
		}

		budgetDecision := req.Budget.RecordCostUSD(chatResp.cost)
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
			resp.Reply = stripReasoningTags(chatResp.Content)
			resp.Messages = messages
			resp.BudgetState = req.Budget.State()
			// Fire-and-forget episodic ingest: the model
			// finished a turn, capture it for dream
			// consolidation. Nil ingester is a no-op; errors are
			// logged and swallowed because memory loss is a
			// soft failure compared to dropping the user's
			// reply.
			a.maybeIngestTurn(ctx, req, chatResp.Content)
			return resp, nil
		}

		// Dispatch each tool call through the Executor. Results come
		// back as tool-role messages for the next LLM round-trip.
		for _, tc := range chatResp.ToolCalls {
			inv, confirmation, err := a.runToolCall(ctx, req, tc)
			if err != nil {
				return nil, fmt.Errorf("tool call %q: %w", tc.Name, err)
			}
			resp.ToolCalls = append(resp.ToolCalls, inv)
			if confirmation != "" {
				resp.NeedsConfirmation = true
				resp.ConfirmationReason = confirmation
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
		resp.Reply = "I hit my tool-call limit for this turn and couldn't complete the task. Try rephrasing or narrowing the request."
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
	out := make([]Message, 0, len(req.ConversationHistory)+2)
	if req.SystemPrompt != "" {
		out = append(out, Message{Role: "system", Content: req.SystemPrompt})
	}
	out = append(out, req.ConversationHistory...)
	userText := decorateWithAttachments(req.Message, req.Attachments)
	if userText != "" {
		out = append(out, Message{Role: "user", Content: userText})
	}
	return out
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
	chatResp, err := a.dispatchWithBackup(ctx, chatReq)
	if err != nil {
		return nil, err
	}

	// For cost accounting the agent needs pricing; Phase 5's agent
	// loop passes it opaquely from the resolver decision. For now,
	// a zero CostRecord is fine: the budget treats zero as "no
	// spend" and no-ops. Phase 5.4 integration (wiring resolver →
	// pricing → agent) will fill this in once the full compose
	// site exists.
	cost := CostRecord{
		ProviderLabel: "",
		Model:         req.Model,
		Usage:         chatResp.Usage,
		CostUSD:       0,
	}

	if a.cfg.Hooks != nil {
		_, _ = a.cfg.Hooks.Dispatch(ctx, types.HookPostLLMCall, map[string]any{
			"turn_id":      req.TurnID,
			"scope":        scopeOfClaims(req.Claims),
			"usage":        chatResp.Usage,
			"finish":       chatResp.FinishReason,
			"tool_calls":   len(chatResp.ToolCalls),
		})
	}

	return &chatWithCost{ChatResponse: chatResp, cost: cost}, nil
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
func (a *Agent) dispatchWithBackup(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	// Single-provider mode: no registry wired, no chain to walk.
	if a.cfg.Providers == nil || a.cfg.PrimaryLabel == "" {
		return a.cfg.Provider.Chat(ctx, req)
	}

	chain := a.cfg.Providers.Chain(a.cfg.PrimaryLabel)
	if len(chain) == 0 {
		return a.cfg.Provider.Chat(ctx, req)
	}

	var lastErr error
	for _, entry := range chain {
		resp, err := entry.Client.Chat(ctx, req)
		if err == nil {
			if lastErr != nil {
				a.cfg.Logger.Info("agent: provider backup succeeded",
					"used_label", entry.Label,
					"prior_error", lastErr.Error())
			}
			return resp, nil
		}
		if !isRetryableProviderError(err, ctx) {
			return nil, err
		}
		a.cfg.Logger.Warn("agent: provider failed; walking backup chain",
			"failed_label", entry.Label, "err", err)
		lastErr = err
	}
	return nil, fmt.Errorf("agent: all providers in chain failed; last error: %w", lastErr)
}

// isRetryableProviderError classifies an LLM call failure as
// transient (worth trying a backup) or permanent (surface
// immediately). Context-cancelled errors are NOT retryable — the
// user intent has changed or the hard-timeout fired, and retrying
// on a backup inside the same cancelled context is wasted.
func isRetryableProviderError(err error, ctx context.Context) bool {
	if err == nil {
		return false
	}
	if ctx.Err() != nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Hard transient signals: rate limits, 5xx, connection
	// refused, i/o timeout. Matching on substring is crude but
	// sufficient — the LLMClient formats these consistently.
	for _, sig := range []string{
		"rate limit", "429", "500", "502", "503", "504",
		"connection refused", "timeout", "deadline",
		"minimax status 1002", // MiniMax RPM limit
	} {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// runToolCall dispatches one tool call through the Executor.
// Executor internally handles policy evaluation, PreToolUse +
// PostToolUse hooks, and the sandbox. On budget exceed, returns a
// non-empty confirmation string; on executor errors that SHOULD
// surface to the model as tool output (non-zero exit, for instance),
// packages them into the ToolInvocation rather than returning an
// error.
func (a *Agent) runToolCall(ctx context.Context, req ProcessMessageRequest, tc ToolCall) (ToolInvocation, string, error) {
	budgetDec := req.Budget.RecordToolCall()
	if budgetDec.Exceeded {
		return ToolInvocation{
			CallID:   tc.ID,
			ToolName: tc.Name,
			Args:     tc.Arguments,
			Error:    "budget exceeded",
		}, fmt.Sprintf("budget exceeded on %s", budgetDec.ExceededOn), nil
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
	if req.Channel != "" {
		params["__channel"] = req.Channel
	}
	if req.ChannelID != "" {
		params["__chat_id"] = req.ChannelID
	}
	if err != nil {
		return ToolInvocation{
			CallID:   tc.ID,
			ToolName: tc.Name,
			Args:     tc.Arguments,
			Error:    fmt.Sprintf("parse args: %v", err),
		}, "", nil
	}

	inv := ToolInvocation{
		CallID:   tc.ID,
		ToolName: tc.Name,
		Args:     tc.Arguments,
	}

	// Skill dispatch takes precedence when the name matches a
	// registered skill. Keeps the executor unaware of skills and
	// lets skill-level errors surface to the model distinctly.
	if a.cfg.Skills != nil && a.cfg.Skills.Has(tc.Name) {
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
			return inv, "", nil
		}
		inv.ExitCode = skillRes.ExitCode
		inv.Output = combineSkillOutputs(skillRes)
		req.Budget.RecordEgressBytes(int64(len(skillRes.Stdout) + len(skillRes.Stderr)))
		return inv, "", nil
	}

	if a.cfg.Executor == nil {
		inv.Error = fmt.Sprintf("tool %q not found (no executor or skill dispatcher registered)", tc.Name)
		return inv, "", nil
	}
	invReq := InvokeRequest{
		ToolName: tc.Name,
		Params:   params,
		Claims:   req.Claims,
		TurnID:   req.TurnID,
	}
	result, err := a.cfg.Executor.Invoke(ctx, invReq)
	if err != nil {
		inv.Error = err.Error()
		return inv, "", nil
	}

	inv.ExitCode = result.ExitCode
	inv.Output = combineOutputs(result)

	req.Budget.RecordEgressBytes(int64(len(result.Stdout) + len(result.Stderr)))

	return inv, "", nil
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
	if inv.Error != "" {
		content = promptgen.WrapContext([]promptgen.ContextBlock{{
			Source:  "tool:" + tc.Name + ":error",
			Trust:   promptgen.TrustUntrusted,
			Content: inv.Error,
		}})
	} else {
		content = promptgen.WrapContext([]promptgen.ContextBlock{{
			Source:  "tool:" + tc.Name + ":output",
			Trust:   promptgen.TrustUntrusted,
			Content: inv.Output,
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
