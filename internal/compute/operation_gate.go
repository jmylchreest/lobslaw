package compute

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RegisterOperationGate installs a trusted semantic gate after hooks and the
// ordinary tool policy. It cannot be selected or rewritten by model arguments.
func (e *Executor) RegisterOperationGate(tool string, gate func(context.Context, *types.Claims, map[string]string) error) {
	e.gateMu.Lock()
	defer e.gateMu.Unlock()
	if e.gated == nil {
		e.gated = map[string]gatedTool{}
	}
	e.gated[tool] = gatedTool{check: gate}
}

// ConfirmExactOperation accepts only the approval attached to this invocation,
// never session grants, broad tool allows or shell label grants.
func ConfirmExactOperation(ctx context.Context, action, resource, summary string, effects types.ToolEffects) error {
	if turnApproved(ctx, action, resource) {
		return nil
	}
	return &ConfirmationRequest{inner: ErrRequireConfirm, Action: action, Resource: resource, Summary: summary, Grantable: false, Labels: ToolEffectLabels(effects)}
}

func ToolEffectLabels(e types.ToolEffects) []commandrisk.RiskLabel {
	var labels []commandrisk.RiskLabel
	switch e.State {
	case types.ToolReads:
		labels = commandrisk.L(commandrisk.LabelReads)
	case types.ToolWrites:
		labels = commandrisk.L(commandrisk.LabelWrites)
	case types.ToolDeletes:
		labels = commandrisk.L(commandrisk.LabelDeletes)
	default:
		return commandrisk.L(commandrisk.LabelUnreadable)
	}
	if e.Reads {
		labels = commandrisk.MergeLabels(labels, commandrisk.L(commandrisk.LabelReads))
	}
	if e.Network {
		labels = commandrisk.MergeLabels(labels, commandrisk.L(commandrisk.LabelNetwork))
	}
	return labels
}
