package tools

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Video, image and speech generation reported NOTHING — not an
// approximation, not a zero with a note, but no cost record at all.
// A turn that rendered a minute of video spent, as far as the budget
// was concerned, nothing: the spend cap could not fire on it and the
// trace could not show it.

func TestANonTokenCallIsPriced(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{
		UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): 0.35},
	}
	got := compute.EstimateModalCost(compute.MeteredUsage(compute.UnitVideoSeconds, 8, compute.BillingBalance), pricing)
	if got != 8*0.35 {
		t.Errorf("8 seconds at $0.35 = %v, want %v", got, 8*0.35)
	}
	if got == 0 {
		t.Error("a per-second provider reported zero, which is the bug this closes")
	}
}

// A video is not a whole number of seconds.
func TestAFractionalQuantityIsPriced(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{
		UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): 0.10},
	}
	got := compute.EstimateModalCost(compute.MeteredUsage(compute.UnitVideoSeconds, 2.5, compute.BillingBalance), pricing)
	if got != 0.25 {
		t.Errorf("2.5 seconds at $0.10 = %v, want 0.25", got)
	}
}

// A turn can be billed BOTH ways: a reply that generates a picture
// costs its tokens and its image. Substituting one for the other would
// under-report whichever came second.
func TestTokenAndUnitCostsAreAdded(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{
		InputUSDPer1K: 1.0,
		UnitUSD:       map[string]float64{string(compute.UnitImages): 0.04},
	}
	// Tokens and units are separate CALLS now, each with its own
	// record — the same turn is billed both ways rather than one
	// compute.ModalUsage carrying both.
	got := compute.EstimateCost(compute.Usage{PromptTokens: 1000}, pricing) +
		compute.EstimateModalCost(compute.MeteredUsage(compute.UnitImages, 2, compute.BillingBalance), pricing)
	want := 1.0 + 0.08
	if got != want {
		t.Errorf("got %v, want %v (tokens plus images)", got, want)
	}
}

// A plan-billed provider has no marginal cost per call. Refusing to
// account for the turn because nobody wrote down a rate would stop the
// turn rather than the billing.
func TestAnUnpricedUnitCostsNothingAndIsNotAnError(t *testing.T) {
	t.Parallel()
	got := compute.EstimateModalCost(compute.MeteredUsage(compute.UnitCredits, 5, compute.BillingBalance), types.ProviderPricing{})
	if got != 0 {
		t.Errorf("got %v; an unpriced unit should contribute nothing", got)
	}
}

// The QUANTITY still travels even when the rate does not, or a
// plan-billed provider's consumption is invisible.
func TestAnUnpricedUnitStillRecordsItsQuantity(t *testing.T) {
	t.Parallel()
	rec := compute.RecordModalCost("vendor", "model-x",
		compute.MeteredUsage(compute.UnitCredits, 5, compute.BillingBalance), types.ProviderPricing{})
	if rec.Usage.Unit != compute.UnitCredits || rec.Usage.Quantity != 5 {
		t.Errorf("usage = %+v; the consumption was not recorded", rec.Usage)
	}
	if rec.CostUSD != 0 {
		t.Errorf("cost = %v, want 0", rec.CostUSD)
	}
}

// An ordinary token-billed call must be unaffected — this change must
// not alter what any existing turn costs.
func TestATokenOnlyCallIsUnchanged(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{InputUSDPer1K: 2, OutputUSDPer1K: 6, CachedUSDPer1K: 0.5}
	usage := compute.Usage{PromptTokens: 1000, CachedTokens: 200, CompletionTokens: 500}
	// 800 fresh at $2/1k + 200 cached at $0.50/1k + 500 out at $6/1k
	want := 1.6 + 0.1 + 3.0
	if got := compute.EstimateCost(usage, pricing); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Metered is what distinguishes the two, so a zero quantity must not
// count as a metered call and pick up a spurious rate.
func TestAPlanDrawCostsNothingButKeepsItsQuantity(t *testing.T) {
	t.Parallel()
	// A plan draw costs nothing however large the quantity, and an
	// unpriced unit costs nothing however it is billed.
	plan := compute.MeteredUsage(compute.UnitVideoSeconds, 60, compute.BillingPlan)
	priced := types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): 1}}
	if got := compute.EstimateModalCost(plan, priced); got != 0 {
		t.Errorf("a plan draw cost %v; a prepaid plan has no marginal cost", got)
	}
	if plan.Quantity != 60 {
		t.Error("the plan draw lost its quantity, which is the meaningful number there")
	}
}

// --- the collector ------------------------------------------------------

// A builtin has no reference to the TurnBudget and should not: it
// would then be able to refuse a turn on the budget's behalf, halfway
// through, from inside a tool.
func TestCollectedCostsReachTheCaller(t *testing.T) {
	t.Parallel()
	ctx, costs := compute.WithCostCollector(context.Background())
	compute.CollectCost(ctx, compute.RecordModalCost("vendor", "m",
		compute.MeteredUsage(compute.UnitImages, 1, compute.BillingBalance),
		types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitImages): 0.04}}))

	got := costs.Drain()
	if len(got) != 1 {
		t.Fatalf("drained %d records", len(got))
	}
	if got[0].CostUSD != 0.04 {
		t.Errorf("cost = %v, want 0.04", got[0].CostUSD)
	}
}

// Drained rather than read: a turn that generates two videos across
// two round-trips must not bill the first one twice.
func TestDrainingEmptiesTheCollector(t *testing.T) {
	t.Parallel()
	ctx, costs := compute.WithCostCollector(context.Background())
	compute.CollectCost(ctx, compute.CostRecord{CostUSD: 1})

	if len(costs.Drain()) != 1 {
		t.Fatal("first drain did not return the record")
	}
	if n := len(costs.Drain()); n != 0 {
		t.Errorf("second drain returned %d records; the same spend would be billed twice", n)
	}
}

// Builtins run from tests, from the scheduler and from turns with no
// budget attached. None of those should have to care.
func TestCollectingWithNoCollectorIsANoOp(t *testing.T) {
	t.Parallel()
	compute.CollectCost(context.Background(), compute.CostRecord{CostUSD: 1})
	var nilCollector *compute.CostCollector
	if got := nilCollector.Drain(); got != nil {
		t.Errorf("draining a nil collector returned %v", got)
	}
}

// --- through the builtins ----------------------------------------------

// The model and the collector are only worth having if the generation
// path actually reports. Asserting on compute.EstimateCost alone would pass
// with the builtins reporting nothing, which is the state this closes.

func TestGeneratingAnImageBillsTheTurn(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	d, _ := compute.MockImageFactory(compute.ImageDriverConfig{})
	if err := RegisterImageBuiltin(b, ImageConfig{
		Driver:   d,
		Resolver: billingResolver(t),
		Label:    "picture-vendor",
		Model:    "img-1",
		Pricing:  types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitImages): 0.04}},
	}); err != nil {
		t.Fatal(err)
	}
	fn, ok := b.Get("generate_image")
	if !ok {
		t.Fatal("generate_image not registered")
	}

	ctx, costs := compute.WithCostCollector(context.Background())
	if _, code, err := fn(ctx, map[string]string{"prompt": "a cat"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}

	got := costs.Drain()
	if len(got) != 1 {
		t.Fatalf("%d cost records; a generated image billed the turn nothing", len(got))
	}
	if got[0].CostUSD != 0.04 {
		t.Errorf("cost = %v, want 0.04", got[0].CostUSD)
	}
	if got[0].Usage.Unit != compute.UnitImages || got[0].Usage.Quantity != 1 {
		t.Errorf("usage = %+v; the unit did not travel", got[0].Usage)
	}
	if got[0].ProviderLabel != "picture-vendor" || got[0].Model != "img-1" {
		t.Errorf("record = %+v; an audit cannot say what was billed", got[0])
	}
}

// TTS is billed by the character of INPUT — not by the second of
// output, which is not known until the audio exists.
func TestSynthesisingSpeechBillsPerCharacter(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	d, _ := compute.MockSpeakFactory(compute.SpeakDriverConfig{})
	if err := RegisterSpeakBuiltin(b, SpeakConfig{
		Driver:   d,
		Resolver: billingResolver(t),
		Label:    "voice-vendor",
		Model:    "tts-1",
		Pricing: types.ProviderPricing{
			UnitUSD: map[string]float64{string(compute.UnitAudioCharacters): 0.00002},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("speak")

	text := "hello there"
	ctx, costs := compute.WithCostCollector(context.Background())
	if _, code, err := fn(ctx, map[string]string{"text": text}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}

	got := costs.Drain()
	if len(got) != 1 {
		t.Fatalf("%d cost records; synthesised speech billed the turn nothing", len(got))
	}
	if got[0].Usage.Quantity != float64(len(text)) {
		t.Errorf("quantity = %v, want %d characters", got[0].Usage.Quantity, len(text))
	}
	if got[0].CostUSD == 0 {
		t.Error("cost is zero for a priced per-character provider")
	}
}

// A failed generation must not bill. The provider did not deliver, and
// charging for it would make an outage look like usage.
func TestAFailedGenerationBillsNothing(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterSpeakBuiltin(b, SpeakConfig{
		Driver:   failingSpeakDriver{},
		Resolver: billingResolver(t),
		Label:    "voice-vendor",
		Pricing: types.ProviderPricing{
			UnitUSD: map[string]float64{string(compute.UnitAudioCharacters): 0.1},
		},
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("speak")

	ctx, costs := compute.WithCostCollector(context.Background())
	if _, _, err := fn(ctx, map[string]string{"text": "hello"}); err == nil {
		t.Fatal("the failing driver reported success")
	}
	if got := costs.Drain(); len(got) != 0 {
		t.Errorf("%d cost records for a generation that failed: %+v", len(got), got)
	}
}

type failingSpeakDriver struct{}

func (failingSpeakDriver) Speak(context.Context, compute.SpeakRequest) (*compute.Artifact, error) {
	return nil, compute.Transient(errorString("the vendor is down"))
}

type errorString string

func (e errorString) Error() string { return string(e) }

// billingResolver writes artifacts into a temp mount, so the cost
// assertions are not entangled with delivery.
func billingResolver(t *testing.T) *compute.ArtifactResolver {
	t.Helper()
	r, _ := newResolver(t)
	return r
}

// --- the loop has to drain it ------------------------------------------

// Everything above tests the collector directly. This tests that the
// TURN drains it into the budget — added after a mutation removing the
// drain failed nothing at all, which is the same gap that hid the
// chain-step pipeline not being called.

// billingDispatcher charges the turn from inside a tool, the way a
// generation builtin does.
type billingDispatcher struct{ cost float64 }

func (billingDispatcher) Has(name string) bool { return name == "make_a_video" }

func (d billingDispatcher) Invoke(ctx context.Context, _ compute.SkillInvokeRequest) (*compute.SkillInvokeResult, error) {
	compute.CollectCost(ctx, compute.RecordModalCost("video-vendor", "veo-1",
		compute.MeteredUsage(compute.UnitVideoSeconds, 8, compute.BillingBalance),
		types.ProviderPricing{UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): d.cost}}))
	return &compute.SkillInvokeResult{ExitCode: 0, Stdout: []byte(`{"ok":true}`)}, nil
}

func TestATurnBillsWhatItsToolsSpent(t *testing.T) {
	t.Parallel()
	provider := compute.NewMockProvider(
		compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "t1", Name: "make_a_video", Arguments: `{}`}}},
		compute.MockResponse{Content: "here is your video"},
	)
	a, err := compute.NewAgent(compute.AgentConfig{
		Provider: provider,
		Skills:   billingDispatcher{cost: 0.25},
	})
	if err != nil {
		t.Fatal(err)
	}
	budget, _ := compute.NewTurnBudget(compute.BudgetCaps{})
	resp, err := a.RunToolCallLoop(context.Background(), compute.ProcessMessageRequest{
		Message: "make me a video",
		Claims:  &types.Claims{UserID: "alice"},
		Budget:  budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.BudgetState.SpendUSD < 2.0 {
		t.Errorf("the turn spent %v; 8 seconds at $0.25 should be $2, so the tool's cost never reached the budget",
			resp.BudgetState.SpendUSD)
	}
}

// And the cap must be able to fire on it — a spend the budget records
// but never acts on is a number, not a limit.
func TestAGenerationCanExceedTheSpendCap(t *testing.T) {
	t.Parallel()
	provider := compute.NewMockProvider(
		compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "t1", Name: "make_a_video", Arguments: `{}`}}},
		compute.MockResponse{Content: "here is your video"},
	)
	a, err := compute.NewAgent(compute.AgentConfig{
		Provider: provider,
		Skills:   billingDispatcher{cost: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	budget, _ := compute.NewTurnBudget(compute.BudgetCaps{MaxSpendUSD: 1})
	resp, err := a.RunToolCallLoop(context.Background(), compute.ProcessMessageRequest{
		Message: "make me an expensive video",
		Claims:  &types.Claims{UserID: "alice"},
		Budget:  budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.NeedsConfirmation {
		t.Errorf("$800 of video against a $1 cap did not stop the turn (spend %v)",
			resp.BudgetState.SpendUSD)
	}
}

// --- the modal model is the only model ---------------------------------

// compute.ModalUsage already existed for exactly this, its own doc saying it
// would "eventually absorb" the token-only compute.Usage. A parallel
// Unit/Quantity was added to compute.Usage before that was noticed, which is
// the two-authorities problem in miniature: two accounts of what a
// turn cost eventually disagree about the answer.

// A prepaid plan has no marginal cost per call, so pricing it as
// though it did would inflate every turn that provider served.
func TestAPlanDrawIsNotPricedButIsCounted(t *testing.T) {
	t.Parallel()
	priced := types.ProviderPricing{
		UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): 1.00},
	}
	plan := compute.MeteredUsage(compute.UnitVideoSeconds, 60, compute.BillingPlan)
	if got := compute.EstimateModalCost(plan, priced); got != 0 {
		t.Errorf("a plan draw cost %v; a prepaid plan has no marginal cost", got)
	}

	// The same quantity against a balance IS priced, or the plan case
	// would be indistinguishable from a missing rate.
	balance := compute.MeteredUsage(compute.UnitVideoSeconds, 60, compute.BillingBalance)
	if got := compute.EstimateModalCost(balance, priced); got != 60 {
		t.Errorf("a balance draw cost %v, want 60", got)
	}

	rec := compute.RecordModalCost("plan-vendor", "veo", plan, priced)
	if rec.Usage.Quantity != 60 {
		t.Error("the plan draw lost its quantity, which is the meaningful number there")
	}
}

// A token call still prices exactly as it did, through the nested
// breakdown. This change must not alter what any existing turn costs.
func TestATokenCallPricesThroughTheModalForm(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{InputUSDPer1K: 2, OutputUSDPer1K: 6}
	tokens := compute.Usage{PromptTokens: 1000, CompletionTokens: 500}
	want := compute.EstimateCost(tokens, pricing)

	got := compute.EstimateModalCost(compute.TokenUsage(tokens, want), pricing)
	if got != want {
		t.Errorf("modal form priced a token call as %v, direct form as %v", got, want)
	}
}

// A token compute.ModalUsage with no breakdown cannot be priced from a
// quantity — total tokens do not say how many were input.
func TestATokenUsageWithNoBreakdownCostsNothing(t *testing.T) {
	t.Parallel()
	pricing := types.ProviderPricing{InputUSDPer1K: 2}
	got := compute.EstimateModalCost(compute.ModalUsage{Unit: compute.UnitTokens, Quantity: 1000}, pricing)
	if got != 0 {
		t.Errorf("got %v; a token count with no input/output split cannot be priced", got)
	}
}
