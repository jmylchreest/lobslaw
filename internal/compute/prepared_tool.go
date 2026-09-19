package compute

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// PreparedToolCall is server-owned continuation data, never an LLM argument or
// a provider wire field. It freezes effective input while a confirmation waits.
// Saved answers never override current policy denials or safety checks.
type PreparedToolCall struct {
	CallID            string
	ToolName          string
	TurnID            string
	OriginalArguments string
	Params            map[string]string
	Approvals         []PreparedApproval
}

func (p *PreparedToolCall) clone() *PreparedToolCall {
	if p == nil {
		return nil
	}
	next := *p
	next.Params = maps.Clone(p.Params)
	next.Approvals = slices.Clone(p.Approvals)
	return &next
}

type preparedConfirmation struct {
	err       error
	params    map[string]string
	approvals []PreparedApproval
}

func (e *preparedConfirmation) Error() string { return e.err.Error() }
func (e *preparedConfirmation) Unwrap() error { return e.err }

func (e *Executor) policyDecision(ctx context.Context, claims *types.Claims, action, resource string) (policy.Decision, error) {
	var dec policy.Decision
	var err error
	switch {
	case e.policy != nil:
		dec, err = e.policy.Evaluate(ctx, claims, action, resource)
	case e.cfg.PolicyFallback != nil:
		dec, err = e.cfg.PolicyFallback(ctx, claims, action, resource)
	default:
		return dec, ErrNoPolicyEngine
	}
	if err != nil {
		return dec, fmt.Errorf("policy evaluate: %w", err)
	}
	return dec, nil
}

// PreparedApproval records an answered gate for this exact prepared call only.
// PolicyAllow consults it only after current policy requests confirmation.
type PreparedApproval struct {
	Action   string
	Resource string
}

type invocationApprovalKey struct{}
type invocationApprovals struct {
	mu         sync.Mutex
	operations []PreparedApproval
}

func withInvocationApprovals(ctx context.Context, previous []PreparedApproval) (context.Context, *invocationApprovals) {
	approvals := &invocationApprovals{operations: slices.Clone(previous)}
	return context.WithValue(ctx, invocationApprovalKey{}, approvals), approvals
}
func (a *invocationApprovals) snapshot() []PreparedApproval {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.operations)
}
func (a *invocationApprovals) approved(ctx context.Context, action, resource string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	op := PreparedApproval{Action: action, Resource: resource}
	if slices.Contains(a.operations, op) {
		return true
	}
	if consumeTurnApproval(ctx, action, resource) {
		a.operations = append(a.operations, op)
		return true
	}
	return false
}
