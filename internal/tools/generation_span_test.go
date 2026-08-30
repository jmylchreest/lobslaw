package tools

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"

	"github.com/jmylchreest/lobslaw/internal/trace"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// trace.Span had Unit and Quantity fields, with a comment explaining
// why they matter — "a zero token count on a call that cost real money
// reads as a free call" — and NOTHING SET THEM. Generation emitted no
// spans at all, so a turn that rendered a video showed a trace with a
// gap where the expensive part happened.

// collectingSink keeps every span for inspection.
type collectingSink struct {
	mu    sync.Mutex
	spans []trace.Span
}

func (s *collectingSink) Write(sp trace.Span) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, sp)
	return nil
}

func (s *collectingSink) Close() error { return nil }

func (s *collectingSink) all() []trace.Span {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]trace.Span, len(s.spans))
	copy(out, s.spans)
	return out
}

// tracedCtx returns a context carrying a recorder, plus the sink and a
// flush func. The recorder is asynchronous, so a test must wait.
func tracedCtx(t *testing.T) (context.Context, *collectingSink, func()) {
	t.Helper()
	sink := &collectingSink{}
	rec := trace.NewRecorder(slog.New(slog.NewTextHandler(io.Discard, nil)), sink)
	ctx := trace.WithTurn(context.Background(), rec, "turn-1")
	return ctx, sink, func() { _ = rec.Close() }
}

func waitForSpans(t *testing.T, sink *collectingSink, n int) []trace.Span {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := sink.all(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d spans after 3s, want %d", len(sink.all()), n)
	return nil
}

func TestAGenerationEmitsASpanCarryingItsUnit(t *testing.T) {
	t.Parallel()
	ctx, sink, done := tracedCtx(t)
	defer done()

	compute.CollectGeneration(ctx, "video-vendor", "veo-1",
		compute.MeteredUsage(compute.UnitVideoSeconds, 8, compute.BillingBalance),
		types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): 0.25}},
		time.Now(), nil)

	spans := waitForSpans(t, sink, 1)
	got := spans[0]
	if got.Kind != trace.KindGeneration {
		t.Errorf("kind = %q", got.Kind)
	}
	if got.Unit != string(compute.UnitVideoSeconds) || got.Quantity != 8 {
		t.Errorf("unit/quantity = %q/%v; the span reports no billable amount", got.Unit, got.Quantity)
	}
	if got.CostUSD != 2 {
		t.Errorf("cost = %v, want 2", got.CostUSD)
	}
	if got.Provider != "video-vendor" || got.Name != "veo-1" {
		t.Errorf("span = %+v; it does not say what was billed", got)
	}
	// Tokens must NOT be invented for a call billed in seconds.
	if got.Usage.PromptTokens != 0 || got.Usage.CompletionTokens != 0 {
		t.Errorf("usage = %+v; a per-second call reported token counts", got.Usage)
	}
}

// THE BOX THIS EXISTS FOR. Without BilledTo a plan-billed call is
// indistinguishable from a free one: both report zero, and only one of
// them consumed something finite. An operator asking why their quota
// ran out would find a trace full of calls that apparently cost
// nothing.
func TestAPlanBilledCallIsMarkedAndCounted(t *testing.T) {
	t.Parallel()
	ctx, sink, done := tracedCtx(t)
	defer done()

	compute.CollectGeneration(ctx, "plan-vendor", "veo-1",
		compute.MeteredUsage(compute.UnitVideoSeconds, 60, compute.BillingPlan),
		types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): 1.00}},
		time.Now(), nil)

	got := waitForSpans(t, sink, 1)[0]
	if got.BilledTo != string(compute.BillingPlan) {
		t.Errorf("billed_to = %q; a plan draw is indistinguishable from a free call", got.BilledTo)
	}
	if got.CostUSD != 0 {
		t.Errorf("cost = %v; a prepaid plan has no marginal cost", got.CostUSD)
	}
	// The quantity is the whole point: it is the only number that says
	// anything was consumed.
	if got.Quantity != 60 {
		t.Errorf("quantity = %v; the quota draw is invisible", got.Quantity)
	}
}

// A failure is what somebody reading a trace is usually looking for,
// so the span is emitted — but it charges nothing, because the
// provider did not deliver and billing for it would make an outage
// look like usage.
func TestAFailedGenerationSpansButDoesNotCharge(t *testing.T) {
	t.Parallel()
	ctx, sink, done := tracedCtx(t)
	defer done()
	ctx, costs := compute.WithCostCollector(ctx)

	compute.CollectGeneration(ctx, "vendor", "m",
		compute.MeteredUsage(compute.UnitImages, 1, compute.BillingBalance),
		types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitImages): 5}},
		time.Now(), errors.New("the vendor is down"))

	got := waitForSpans(t, sink, 1)[0]
	if got.Outcome != trace.OutcomeAborted {
		t.Errorf("outcome = %q for a failed generation", got.Outcome)
	}
	if got.Error == "" {
		t.Error("the span carries no reason")
	}
	if got.CostUSD != 0 {
		t.Errorf("cost = %v; a failed generation was charged for", got.CostUSD)
	}
	if recs := costs.Drain(); len(recs) != 0 {
		t.Errorf("%d cost records for a generation that failed", len(recs))
	}
}

// The budget and the trace are two accounts of the same spend. One
// call writes both, so a modality cannot end up with a span and no
// cost record or the reverse.
func TestOneCallFeedsBothTheBudgetAndTheTrace(t *testing.T) {
	t.Parallel()
	ctx, sink, done := tracedCtx(t)
	defer done()
	ctx, costs := compute.WithCostCollector(ctx)

	compute.CollectGeneration(ctx, "vendor", "m",
		compute.MeteredUsage(compute.UnitImages, 2, compute.BillingBalance),
		types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitImages): 0.04}},
		time.Now(), nil)

	spans := waitForSpans(t, sink, 1)
	recs := costs.Drain()
	if len(recs) != 1 {
		t.Fatalf("%d cost records", len(recs))
	}
	if recs[0].CostUSD != spans[0].CostUSD {
		t.Errorf("budget says %v, trace says %v — two accounts of one spend",
			recs[0].CostUSD, spans[0].CostUSD)
	}
	if float64(recs[0].Usage.Quantity) != spans[0].Quantity {
		t.Errorf("budget quantity %v, trace quantity %v", recs[0].Usage.Quantity, spans[0].Quantity)
	}
}

// Tracing off must not break a generation. A deployment with no
// recorder is the common case.
func TestAGenerationWorksWithNoRecorder(t *testing.T) {
	t.Parallel()
	ctx, costs := compute.WithCostCollector(context.Background())
	compute.CollectGeneration(ctx, "vendor", "m",
		compute.MeteredUsage(compute.UnitImages, 1, compute.BillingBalance), types.ProviderPricing{},
		time.Now(), nil)
	if len(costs.Drain()) != 1 {
		t.Error("the cost was lost when tracing was off")
	}
}

// --- through the builtin -----------------------------------------------

// Asserting on compute.CollectGeneration alone would pass with the builtins
// never calling it, which is the state this closes.
func TestGeneratingAnImageEmitsASpan(t *testing.T) {
	t.Parallel()
	ctx, sink, done := tracedCtx(t)
	defer done()

	b := NewBuiltins()
	d, _ := compute.MockImageFactory(compute.ImageDriverConfig{})
	if err := RegisterImageBuiltin(b, ImageConfig{
		Driver: d, Resolver: billingResolver(t),
		Label: "picture-vendor", Model: "img-1",
		Pricing: types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitImages): 0.04}},
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("generate_image")
	if _, code, err := fn(ctx, map[string]string{"prompt": "a cat"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}

	got := waitForSpans(t, sink, 1)[0]
	if got.Kind != trace.KindGeneration || got.Unit != string(compute.UnitImages) {
		t.Errorf("span = %+v; generating an image left a gap in the trace", got)
	}
}
