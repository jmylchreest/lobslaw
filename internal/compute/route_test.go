package compute

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The turn has to actually START where the chain says.
//
// Resolving a chain and then dispatching to PrimaryLabel anyway is
// precisely the state this replaced: chains parsed, validated, logged,
// and inert. So these tests assert on WHICH PROVIDER WAS CALLED, not on
// what the resolver returned.

func routedAgent(t *testing.T, chains []config.ChainConfig, judgment string) (*Agent, *[]string) {
	t.Helper()
	calls := &[]string{}
	reg := NewProviderRegistry()
	for _, label := range []string{"cheap", "big"} {
		reg.Register(ProviderEntry{
			Label:     label,
			TrustTier: types.TrustPrivate,
			Client:    &scriptedProvider{label: label, calls: calls},
		})
	}
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{
			{Label: "cheap", Endpoint: "https://example.invalid", Model: "s", TrustTier: types.TrustPrivate},
			{Label: "big", Endpoint: "https://example.invalid", Model: "l", TrustTier: types.TrustPrivate},
		},
		Chains: chains,
	}
	resolver, err := NewResolver(cfg)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	var judge *Judge
	if judgment != "" {
		judge = NewJudge(&scriptedLLM{reply: judgment}, "tiny", 0, slog.Default())
	}
	a := &Agent{cfg: AgentConfig{
		Provider:     &scriptedProvider{label: "unused", calls: calls},
		Providers:    reg,
		PrimaryLabel: "cheap",
		Resolver:     resolver,
		Judge:        judge,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	return a, calls
}

func deepChainCfg() config.ChainConfig {
	return config.ChainConfig{
		Label:   "deep",
		Trigger: config.ChainTriggerConfig{MinComplexity: 70},
		Steps:   []config.ChainStepConfig{{Provider: "big"}},
	}
}

// A hard turn is dispatched to the chain's provider, not to
// PrimaryLabel. This is the whole of "chains route".
func TestAResolvedChainChangesWhichProviderIsCalled(t *testing.T) {
	t.Parallel()
	a, calls := routedAgent(t, []config.ChainConfig{deepChainCfg()},
		`{"complexity": 90, "hint": "deep"}`)

	ctx := WithRoute(context.Background(),
		a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "prove this terminates"}))
	if _, err := a.dispatchWithBackup(ctx, ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "big" {
		t.Errorf("calls = %v; the turn did not start where the chain said", *calls)
	}
}

// An easy turn still starts at the primary. A router that sends
// everything to the expensive provider has not routed anything.
func TestASimpleTurnStillStartsAtThePrimary(t *testing.T) {
	t.Parallel()
	a, calls := routedAgent(t, []config.ChainConfig{deepChainCfg()},
		`{"complexity": 5, "hint": "fast"}`)

	ctx := WithRoute(context.Background(),
		a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "hello"}))
	if _, err := a.dispatchWithBackup(ctx, ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "cheap" {
		t.Errorf("calls = %v, want the turn to start at the primary", *calls)
	}
}

// An explicit hint routes without paying for a preflight at all.
func TestAnExplicitHintRoutesWithoutAJudge(t *testing.T) {
	t.Parallel()
	a, calls := routedAgent(t, []config.ChainConfig{deepChainCfg()}, "")

	req := ProcessMessageRequest{Message: "anything", Hint: HintDeep}
	ctx := WithRoute(context.Background(), a.resolveRoute(context.Background(), req))
	if _, err := a.dispatchWithBackup(ctx, ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "big" {
		t.Errorf("calls = %v; an explicit hint did not route", *calls)
	}
}

// No resolver is the state every deployment was in before this. The
// turn must behave exactly as it did.
func TestWithoutAResolverNothingChanges(t *testing.T) {
	t.Parallel()
	a, calls := routedAgent(t, nil, "")
	a.cfg.Resolver = nil

	if got := a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "x"}); got != nil {
		t.Fatalf("route = %+v, want nil without a resolver", got)
	}
	if _, err := a.dispatchWithBackup(context.Background(), ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "cheap" {
		t.Errorf("calls = %v, want the primary", *calls)
	}
}

// A turn with no route on its context is a turn that starts at the
// primary — not one that panics on a nil.
func TestAContextWithNoRouteStartsAtThePrimary(t *testing.T) {
	t.Parallel()
	a, calls := routedAgent(t, []config.ChainConfig{deepChainCfg()}, "")
	if _, err := a.dispatchWithBackup(context.Background(), ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "cheap" {
		t.Errorf("calls = %v, want the primary", *calls)
	}
}

// The route carries WHY, so an operator can see which chain fired
// rather than inferring it from which provider answered.
func TestTheRouteRecordsWhy(t *testing.T) {
	t.Parallel()
	a, _ := routedAgent(t, []config.ChainConfig{deepChainCfg()},
		`{"complexity": 90, "hint": "deep"}`)
	got := a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "hard"})
	if got == nil {
		t.Fatal("no route")
	}
	if got.ChainLabel != "deep" || got.Reason == "" {
		t.Errorf("route = %+v; an operator cannot see why it fired", got)
	}
	if got.Judgment.Complexity != 90 {
		t.Errorf("judgment = %+v; the signal was not carried", got.Judgment)
	}
}

// The resolver's "nobody matched" fallback picks the highest-trust
// provider, breaking ties alphabetically. That answers "who COULD
// serve this", not "where should this start" — with two equal-trust
// providers it silently moves every unmatched turn off roles.main and
// onto whichever label happens to sort first.
//
// Found by TestASimpleTurnStillStartsAtThePrimary, which routed a
// complexity-5 greeting to the expensive provider.
func TestTheSynthesisedFallbackDoesNotReroute(t *testing.T) {
	t.Parallel()
	// No chains at all: every turn goes through the fallback path.
	a, calls := routedAgent(t, nil, `{"complexity": 5, "hint": "fast"}`)

	got := a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "hello"})
	if got != nil {
		t.Errorf("route = %+v; an unmatched turn should not be rerouted", got)
	}
	ctx := WithRoute(context.Background(), got)
	if _, err := a.dispatchWithBackup(ctx, ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "cheap" {
		t.Errorf("calls = %v; the fallback rerouted a turn off the primary", *calls)
	}
}
