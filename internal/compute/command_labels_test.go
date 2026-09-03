package compute

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
)

func TestCommandLabelsContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, ok := CommandLabelsFrom(ctx); ok {
		t.Error("a bare context reports labels")
	}
	// An invalid label is dropped rather than carried: a rule
	// conditioned on labels must not match something nobody classified.
	if _, ok := CommandLabelsFrom(WithCommandLabels(ctx, commandrisk.L(commandrisk.RiskLabel("bananas")))); ok {
		t.Error("an invalid label was stored")
	}
	got, ok := CommandLabelsFrom(WithCommandLabels(ctx, commandrisk.L(commandrisk.LabelWrites)))
	if !ok || !(len(got) == 1 && got[0] == commandrisk.LabelWrites) {
		t.Errorf("CommandLabelsFrom = %v/%v, want writes/true", got, ok)
	}
}
