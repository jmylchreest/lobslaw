package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

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
const DefaultMaxToolLoops = 16

// AgentConfig configures the agent loop.
type AgentConfig struct {
	// Provider is the LLM the agent calls. Required — nil yields
	// ErrNoLLMProvider at Run time.
	Provider LLMProvider

	// Executor runs tool invocations. Required for any turn that
	// involves tool calls (i.e. most of them).
	Executor *Executor

	// Hooks dispatches lifecycle events (PreLLMCall, PostLLMCall,
	// PreToolUse, PostToolUse). May be nil — all hook calls become
	// no-ops when unset.
	Hooks HookDispatcher

	// MaxToolLoops bounds tool-call round-trips per turn. 0 →
	// DefaultMaxToolLoops.
	MaxToolLoops int

	// Logger is used for structured log entries. Nil → slog.Default().
	Logger *slog.Logger
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
	return a.runLoop(ctx, req, a.seedMessages(req), &ProcessMessageResponse{})
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
			resp.Reply = chatResp.Content
			resp.Messages = messages
			resp.BudgetState = req.Budget.State()
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

	return nil, fmt.Errorf("%w: %d", ErrMaxToolLoops, a.cfg.MaxToolLoops)
}

// seedMessages builds the initial message list from the system
// prompt + conversation history + the user's current message.
func (a *Agent) seedMessages(req ProcessMessageRequest) []Message {
	out := make([]Message, 0, len(req.ConversationHistory)+2)
	if req.SystemPrompt != "" {
		out = append(out, Message{Role: "system", Content: req.SystemPrompt})
	}
	out = append(out, req.ConversationHistory...)
	if req.Message != "" {
		out = append(out, Message{Role: "user", Content: req.Message})
	}
	return out
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
	chatResp, err := a.cfg.Provider.Chat(ctx, chatReq)
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
	if err != nil {
		return ToolInvocation{
			CallID:   tc.ID,
			ToolName: tc.Name,
			Args:     tc.Arguments,
			Error:    fmt.Sprintf("parse args: %v", err),
		}, "", nil
	}

	invReq := InvokeRequest{
		ToolName: tc.Name,
		Params:   params,
		Claims:   req.Claims,
		TurnID:   req.TurnID,
	}
	result, err := a.cfg.Executor.Invoke(ctx, invReq)
	inv := ToolInvocation{
		CallID:   tc.ID,
		ToolName: tc.Name,
		Args:     tc.Arguments,
	}
	if err != nil {
		// Executor surfaced a control-flow error (policy denied, hook
		// blocked, etc). Package into the ToolInvocation so the model
		// sees the rejection reason.
		inv.Error = err.Error()
		return inv, "", nil
	}

	inv.ExitCode = result.ExitCode
	inv.Output = combineOutputs(result)

	req.Budget.RecordEgressBytes(int64(len(result.Stdout) + len(result.Stderr)))

	return inv, "", nil
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
