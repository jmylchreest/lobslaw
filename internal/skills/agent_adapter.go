package skills

import (
	"context"
	"errors"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// AgentAdapter satisfies compute.SkillDispatcher over an existing
// Registry + Invoker pair. Lets the agent check "is this a skill?"
// and dispatch through Invoker without the agent package importing
// internal/skills.
type AgentAdapter struct {
	registry *Registry
	invoker  *Invoker
}

// NewAgentAdapter returns the bridge. Both registry and invoker
// required; either nil is a boot-time misconfiguration.
func NewAgentAdapter(registry *Registry, invoker *Invoker) (*AgentAdapter, error) {
	if registry == nil {
		return nil, errors.New("skills.AgentAdapter: registry required")
	}
	if invoker == nil {
		return nil, errors.New("skills.AgentAdapter: invoker required")
	}
	return &AgentAdapter{registry: registry, invoker: invoker}, nil
}

// Has checks registry membership. Called by the agent before each
// tool-call to decide whether to route to skills vs the executor.
func (a *AgentAdapter) Has(name string) bool {
	_, err := a.registry.Get(name)
	return err == nil
}

// Invoke translates the agent's request shape into the invoker's
// shape + back. Errors propagate untouched so the agent surfaces
// them to the model as tool-call errors.
func (a *AgentAdapter) Invoke(ctx context.Context, req compute.SkillInvokeRequest) (*compute.SkillInvokeResult, error) {
	res, err := a.invoker.Invoke(ctx, InvokeRequest{
		SkillName: req.Name,
		Params:    req.Params,
	})
	if err != nil {
		return nil, err
	}
	return &compute.SkillInvokeResult{
		ExitCode: res.ExitCode,
		Stdout:   res.Stdout,
		Stderr:   res.Stderr,
	}, nil
}
