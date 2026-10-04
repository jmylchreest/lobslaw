package compute

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestExactOperationCannotBorrowOtherApproval(t *testing.T) {
	effects := types.ToolEffects{State: types.ToolWrites, Network: true}
	for _, ctx := range []context.Context{context.Background(), WithTurnApproval(context.Background(), "tool:exec", "calendar_event_create"), WithTurnApproval(context.Background(), "calendar:change", "old-payload")} {
		err := ConfirmExactOperation(ctx, "calendar:change", "new-payload", "exact event", effects)
		var prompt *ConfirmationRequest
		if !errors.As(err, &prompt) || prompt.Grantable || !errors.Is(err, ErrRequireConfirm) {
			t.Fatalf("missing exact prompt: %v", err)
		}
		if !slices.Contains(prompt.Labels, commandrisk.LabelWrites) || !slices.Contains(prompt.Labels, commandrisk.LabelNetwork) {
			t.Fatalf("lost effects: %v", prompt.Labels)
		}
	}
	if err := ConfirmExactOperation(WithTurnApproval(context.Background(), "calendar:change", "new-payload"), "calendar:change", "new-payload", "exact event", effects); err != nil {
		t.Fatal(err)
	}
	read := ToolEffectLabels(types.ToolEffects{State: types.ToolReads, Network: true})
	if !slices.Contains(read, commandrisk.LabelReads) || !slices.Contains(read, commandrisk.LabelNetwork) || slices.Contains(read, commandrisk.LabelWrites) {
		t.Fatalf("read labels: %v", read)
	}
}
