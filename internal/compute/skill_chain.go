package compute

import (
	"context"
	"fmt"
)

// SkillDispatcherChain composes multiple SkillDispatcher
// implementations so skills + MCP tools (and any future dispatcher
// families) can coexist. First match wins on Has; Invoke routes to
// whichever dispatcher claimed the name.
//
// Nil dispatchers in the slice are tolerated (skipped). Empty
// chain → no skill dispatch at all, agent falls through to the
// Executor for every tool call.
type SkillDispatcherChain struct {
	dispatchers []SkillDispatcher
}

// NewSkillDispatcherChain returns a chain that tries dispatchers
// left-to-right. Pass the most-specific (e.g. local skills) first
// so generic fallbacks (e.g. MCP) come after.
func NewSkillDispatcherChain(dispatchers ...SkillDispatcher) *SkillDispatcherChain {
	return &SkillDispatcherChain{dispatchers: dispatchers}
}

// Has reports whether ANY chained dispatcher claims the name.
func (c *SkillDispatcherChain) Has(name string) bool {
	for _, d := range c.dispatchers {
		if d == nil {
			continue
		}
		if d.Has(name) {
			return true
		}
	}
	return false
}

// Invoke routes to the first dispatcher that claims name. If none
// claim, returns a descriptive error — the agent treats this the
// same as any other skill-invocation failure.
func (c *SkillDispatcherChain) Invoke(ctx context.Context, req SkillInvokeRequest) (*SkillInvokeResult, error) {
	for _, d := range c.dispatchers {
		if d == nil {
			continue
		}
		if d.Has(req.Name) {
			return d.Invoke(ctx, req)
		}
	}
	return nil, fmt.Errorf("skill chain: no dispatcher claims %q", req.Name)
}
