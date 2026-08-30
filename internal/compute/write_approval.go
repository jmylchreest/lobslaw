package compute

import (
	"context"
	"errors"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Staging what the agent writes to memory.
//
// Memory that silently self-modifies and cannot be inspected is a
// trust problem for a privacy-first product. `lobslaw memory list` and
// the consolidation log answer "what happened"; neither answers "may
// this happen", and the write lands either way.
//
// So a tool can be marked as needing approval before it runs. The gate
// is a POLICY question rather than a branch inside the tool, which is
// what makes the answer reusable: a session grant covers the rest of
// the conversation, an "always" mints a visible and revocable rule,
// and an operator who wants something narrower writes an ordinary
// policy rule that outranks the config default.
//
// The alternative — a check inside memory_write consulting a bool —
// would have needed its own notion of "already approved", and would
// have grown a second, subtly different approval system beside the one
// R2 built.

// ApprovalAction is the policy action a gated write is checked under.
//
// Distinct from tool:exec, deliberately. A deployment allows tool:exec
// on memory_write in the ordinary way — otherwise the tool could not
// run at all — so reusing that action would mean the allow rule
// already in place silently satisfied the gate.
const ApprovalAction = "memory:write"

// gatedTool is what an extra approval check needs to know.
type gatedTool struct {
	action   string
	resource string
	// resolve derives the resource THIS CALL is about from its
	// parameters, for a gate whose question is about the arguments
	// rather than the tool name. Nil means the fixed resource above is
	// the whole answer.
	//
	// grantable=false means confirmable but not generalisable: there
	// is no class a grant could name, so the channel is told the
	// resource is empty and offers no scope button.
	resolve func(map[string]string) (string, bool)
	// summarise turns the call's parameters into something a person
	// can decide about. A confirmation that says only "the agent wants
	// to write a memory" is one nobody can answer usefully, so they
	// answer it reflexively — which is worse than not asking.
	summarise func(map[string]string) string
}

// RequireApproval marks a tool as needing confirmation before it runs.
//
// Additive to the ordinary tool:exec check rather than a replacement:
// a denied tool stays denied, and this only ever adds a question.
//
// The ACTION is not a parameter. Passing one would let a caller supply
// "tool:exec" — the action every deployment already allows for this
// tool, since otherwise it could not run — and the gate would be
// silently satisfied by a rule that has nothing to do with it. There is
// no configuration in which that is the right thing to pass, so it is
// not offered.
func (e *Executor) RequireApproval(tool, resource string, summarise func(map[string]string) string) {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	if e.gated == nil {
		e.gated = map[string]gatedTool{}
	}
	e.gated[tool] = gatedTool{action: ApprovalAction, resource: resource, summarise: summarise}
}

// RequireCommandApproval marks a tool whose approval is about its
// PARAMETERS rather than its name.
//
// The ACTION is not a parameter here for the same reason it is not one
// on RequireApproval: a caller could pass "tool:exec", which every
// deployment already allows for this tool from the wire_seeds.go
// default, and the gate would be satisfied by a rule that has nothing
// to do with it.
func (e *Executor) RequireCommandApproval(tool string, resolve func(map[string]string) (string, bool), summarise func(map[string]string) string) {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	if e.gated == nil {
		e.gated = map[string]gatedTool{}
	}
	e.gated[tool] = gatedTool{action: ShellAction, resolve: resolve, summarise: summarise}
}

// approvalFor returns the gate for a tool, if it has one.
func (e *Executor) approvalFor(tool string) (gatedTool, bool) {
	e.gateMu.RLock()
	defer e.gateMu.RUnlock()
	g, ok := e.gated[tool]
	return g, ok
}

// CheckGate runs the extra gate, if the tool has one.
//
// Returns ErrRequireConfirm carrying a summary of WHAT is about to
// happen, because the decision is about the content and a prompt that
// withholds it cannot be answered — and carrying the gate's own action
// and resource, because the channel has to record the answer against
// the operation that was actually asked about.
func (e *Executor) CheckGate(ctx context.Context, claims *types.Claims, tool string, params map[string]string) error {
	gate, ok := e.approvalFor(tool)
	if !ok {
		return nil
	}
	resource, grantable := gate.resource, true
	if gate.resolve != nil {
		resource, grantable = gate.resolve(params)
	}
	err := e.PolicyAllow(ctx, claims, gate.action, resource)
	if err == nil {
		return nil
	}
	// A confirmation gets the summary appended so the prompt says what
	// is being asked about. Any other outcome — a deny, an engine
	// failure — passes through untouched: adding content to a denial
	// would put it in front of somebody who is not being asked to
	// decide.
	if !errors.Is(err, ErrRequireConfirm) {
		return err
	}
	req := &ConfirmationRequest{
		inner: err, Action: gate.action, Resource: resource, Grantable: grantable,
	}
	if gate.summarise != nil {
		req.Summary = gate.summarise(params)
	}
	return req
}

// ConfirmationRequest is a confirmation that knows what it is about.
//
// The gate asks under its OWN action — memory:write, not the tool:exec
// the executor asked first — so the channel has to be told that action.
// Without it the answer is recorded against the wrong operation: an
// "approve for this chat" grants tool:exec/memory_write, which the gate
// never consults, and an "always" mints an allow rule for tool:exec
// that wire_seeds.go already seeded on every node. Both look like they
// worked, and the same prompt returns forever.
//
// Action empty means the caller keeps whatever it was already going to
// report. Resource empty means confirmable but not grantable — there is
// no class a grant could name, so the channel offers no scope button.
type ConfirmationRequest struct {
	inner    error
	Action   string
	Resource string
	Summary  string
	// Grantable reports whether an answer to this may be REMEMBERED.
	//
	// Separate from Resource because the two questions are different,
	// and conflating them cost a bug: a shell command with no stable
	// form was reported with a blank resource so the channels would
	// hide the scope buttons, and then the turn approval had nothing to
	// match on, so approving it once resumed straight back into the
	// same prompt. The resource is what policy evaluates and what the
	// turn approval keys on; this is what the channel offers.
	Grantable bool
}

func (c *ConfirmationRequest) Error() string {
	if c.Summary == "" {
		return c.inner.Error()
	}
	return c.inner.Error() + ": " + c.Summary
}

func (c *ConfirmationRequest) Unwrap() error { return c.inner }

// MemoryWriteSummary renders a memory_write call for a confirmation
// prompt.
//
// Content DOES appear here, unlike in a trace span, and the difference
// is the audience: a span goes to whatever telemetry the operator
// runs, while this goes to the person being asked to approve the
// write. Withholding it would make the question unanswerable.
//
// Truncated, because a confirmation is a prompt and a prompt that is
// three screens long is one nobody reads to the end.
func MemoryWriteSummary(params map[string]string) string {
	event := strings.TrimSpace(params["event"])
	if event == "" {
		return ""
	}
	const cap = 200
	if len([]rune(event)) > cap {
		event = string([]rune(event)[:cap-1]) + "…"
	}
	var b strings.Builder
	b.WriteString("remember: ")
	b.WriteString(event)
	if tags := strings.TrimSpace(params["tags"]); tags != "" && tags != "[]" {
		b.WriteString(" (tags ")
		b.WriteString(tags)
		b.WriteString(")")
	}
	return b.String()
}

// WriteApprovalDefault is the policy rule the config flag installs.
//
// A rule rather than a hardcoded branch, so it composes: an operator
// can override it with anything of higher priority, an approval can
// mint an allow that outranks it, and it shows up wherever rules show
// up rather than being invisible behaviour.
//
// Priority is deliberately the lowest the type allows. A default that
// could outrank an operator's rule would not be a default.
func WriteApprovalDefault() types.PolicyRule {
	return types.PolicyRule{
		ID:       "config:memory.write_approval",
		Subject:  "*",
		Action:   ApprovalAction,
		Resource: "*",
		Effect:   types.EffectRequireConfirmation,
		Priority: -1 << 30,
	}
}
