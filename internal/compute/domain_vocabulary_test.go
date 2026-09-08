package compute

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// DOMAIN ROUTING WAS A LOTTERY.
//
// `trigger.domains = ["code"]` fires when the judge happens to emit
// `code`, and not when it emits `coding`, `software` or
// `api-integration`. Two models given the same sentence agreed on zero
// tags: one answered api-integration, task-scheduling, alert-systems;
// the other weather, automation, scheduling, alerts.
//
// min_complexity is a number and behaves. Domains are the only way to
// route on SUBJECT, which is the trust-sensitive one: a `legal` rule
// that silently fails to match sends the turn to a public-tier provider
// instead of the private one the operator asked for. A routing rule that
// fails open and silently is worse than one that does not exist.

// judgedAgent wires an agent whose preflight is a scripted model, so a
// test can read the prompt that actually went out as well as the route
// that came back.
func judgedAgent(t *testing.T, chains []config.ChainConfig, reply string) (*Agent, *scriptedLLM) {
	t.Helper()
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
	llm := &scriptedLLM{reply: reply}
	a := &Agent{cfg: AgentConfig{
		Provider:     &scriptedProvider{label: "cheap", calls: &[]string{}},
		PrimaryLabel: "cheap",
		Resolver:     resolver,
		Judge:        NewJudge(llm, "tiny", 0, slog.Default()),
		Logger:       slog.New(slog.DiscardHandler),
	}}
	return a, llm
}

// domainChain routes on subject alone.
func domainChain(label string, domains ...string) config.ChainConfig {
	return config.ChainConfig{
		Label:   label,
		Trigger: config.ChainTriggerConfig{Domains: domains},
		Steps:   []config.ChainStepConfig{{Provider: "big"}},
	}
}

// systemPromptSent returns the instruction the preflight provider was
// actually given. Asserting on what a prompt helper RETURNS is not the
// same as asserting on what goes out.
func systemPromptSent(t *testing.T, llm *scriptedLLM) string {
	t.Helper()
	if llm.calls != 1 {
		t.Fatalf("the preflight was called %d times; there is no prompt to read", llm.calls)
	}
	for _, m := range llm.seen.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	t.Fatal("the preflight request carried no system message")
	return ""
}

// The vocabulary in force is the operator's, so the prompt has to carry
// it. A model asked for "the subject area" answers in whatever words it
// likes, and the words it likes are not config keys.
func TestThePreflightIsOfferedOnlyTheDeclaredDomains(t *testing.T) {
	t.Parallel()
	a, llm := judgedAgent(t, []config.ChainConfig{domainChain("private-tier", "conveyancing", "medical")},
		`{"complexity": 40, "domains": ["conveyancing"]}`)

	a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "review this contract"})

	prompt := systemPromptSent(t, llm)
	for _, want := range []string{"conveyancing", "medical"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the prompt does not offer %q, which a chain routes on:\n%s", want, prompt)
		}
	}
	// The old prompt named its own examples. Offering a tag no chain
	// declares invites exactly the answer that cannot route.
	if strings.Contains(prompt, "finance") {
		t.Errorf("the prompt offers %q, which no chain declares:\n%s", "finance", prompt)
	}
}

// A tag outside the vocabulary is not a weaker signal, it is no signal:
// nothing can route on it. Carrying it onto the route puts it in the
// operator's log as though it had meant something.
func TestATagNoChainDeclaresDoesNotReachTheRoute(t *testing.T) {
	t.Parallel()
	// The deep chain routes on complexity, so there IS a route to
	// inspect; what the judgment carries is the question.
	chains := []config.ChainConfig{
		{
			Label:   "deep",
			Trigger: config.ChainTriggerConfig{MinComplexity: 70},
			Steps:   []config.ChainStepConfig{{Provider: "big"}},
		},
		domainChain("private-tier", "legal"),
	}
	a, _ := judgedAgent(t, chains,
		`{"complexity": 90, "domains": ["api-integration", "task-scheduling", "alert-systems"]}`)

	got := a.resolveRoute(context.Background(),
		ProcessMessageRequest{Message: "add a weather skill that checks every few hours"})
	if got == nil {
		t.Fatal("no route; this test needs one to read the judgment off")
	}
	if len(got.Judgment.Domains) != 0 {
		t.Errorf("domains = %v; tags no chain declares were carried as though they could route",
			got.Judgment.Domains)
	}
}
