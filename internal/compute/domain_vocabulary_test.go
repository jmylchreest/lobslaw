package compute

import (
	"context"
	"log/slog"
	"slices"
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
		Judge:        NewJudge(llm, "tiny", resolver, 0, slog.Default()),
		Logger:       slog.New(slog.DiscardHandler),
	}}
	return a, llm
}

// resolverOver builds a resolver whose chains declare these domains.
func resolverOver(t *testing.T, vocabulary []string) *Resolver {
	t.Helper()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("p", types.TrustPublic)},
	}
	if len(vocabulary) > 0 {
		cfg.Chains = []config.ChainConfig{{
			Label:   "subject",
			Steps:   []config.ChainStepConfig{{Provider: "p"}},
			Trigger: config.ChainTriggerConfig{Domains: vocabulary},
		}}
	}
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatalf("resolver: %v", err)
	}
	return r
}

// judgeOver builds a judge the way a node does: from a resolver, over
// the domains some chain declares. Handing it a vocabulary directly
// would prove the filter works and not that the operator's config
// reaches it.
func judgeOver(t *testing.T, llm LLMProvider, vocabulary []string) *Judge {
	t.Helper()
	return NewJudge(llm, "tiny", resolverOver(t, vocabulary), 0, slog.Default())
}

// parsed reads a reply against a vocabulary, for the tests that are
// about parsing rather than about routing.
func parsed(reply string, vocabulary ...string) Judgment {
	j, _ := parseJudgment(reply, newDomainSet(vocabulary), slog.Default())
	return j
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

// The vocabulary is every trigger's domains together, because any of
// them can select a chain. Overlap and repetition are how a real config
// reads: two chains both caring about "legal" is one subject, not two.
func TestTheVocabularyIsTheUnionOfEveryTrigger(t *testing.T) {
	t.Parallel()
	cfg := &config.ComputeConfig{
		Providers: []config.ProviderConfig{providerAt("big", types.TrustPrivate)},
		Chains: []config.ChainConfig{
			domainChain("private-tier", "legal", "medical", "legal"),
			domainChain("money", "Finance", " legal "),
			{
				Label:   "deep",
				Trigger: config.ChainTriggerConfig{MinComplexity: 70},
				Steps:   []config.ChainStepConfig{{Provider: "big"}},
			},
		},
	}
	r, err := NewResolver(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"finance", "legal", "medical"}
	if got := r.DomainVocabulary(); !slices.Equal(got, want) {
		t.Errorf("vocabulary = %v, want %v", got, want)
	}
}

// The fix must not cost the routing that already worked: a declared tag
// the judge names still sends the turn to the chain that asked for it.
func TestADeclaredTagStillRoutes(t *testing.T) {
	t.Parallel()
	a, calls := routedAgent(t, []config.ChainConfig{domainChain("private-tier", "legal")},
		`{"complexity": 10, "domains": ["legal"]}`)

	ctx := WithRoute(context.Background(),
		a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "review this contract"}))
	if _, err := a.dispatchWithBackup(ctx, ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "big" {
		t.Errorf("calls = %v; a declared domain no longer routes", *calls)
	}
}

// The near-miss is the whole complaint: `code` is declared, the model
// says `coding`, and the turn goes to a provider the operator did not
// choose for it.
func TestANearMissDoesNotMatchATrigger(t *testing.T) {
	t.Parallel()
	a, _ := judgedAgent(t, []config.ChainConfig{domainChain("code-model", "code")},
		`{"complexity": 10, "domains": ["coding"]}`)

	if got := a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "fix this test"}); got != nil {
		t.Errorf("route = %+v; a tag no chain declares selected one", got)
	}
}

// An operator who wrote "Legal" wrote a rule that could never fire: the
// judgment side is lowercased on the way in, so the two spellings could
// not meet. Both sides normalise now.
func TestADeclaredTagMatchesWhateverCaseItWasWrittenIn(t *testing.T) {
	t.Parallel()
	a, calls := routedAgent(t, []config.ChainConfig{domainChain("private-tier", " Legal ")},
		`{"complexity": 10, "domains": ["legal"]}`)

	ctx := WithRoute(context.Background(),
		a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "review this contract"}))
	if _, err := a.dispatchWithBackup(ctx, ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) == 0 || (*calls)[0] != "big" {
		t.Errorf("calls = %v; a capitalised trigger domain is still a rule that cannot fire", *calls)
	}
}

// Nothing declares a domain in most deployments. Asking for a tag out of
// an empty list would be worse than the prompt it replaced, and a tag
// nothing routes on is not worth the tokens.
func TestWithNoDeclaredDomainsThePromptAsksForNone(t *testing.T) {
	t.Parallel()
	a, llm := judgedAgent(t, []config.ChainConfig{{
		Label:   "deep",
		Trigger: config.ChainTriggerConfig{MinComplexity: 70},
		Steps:   []config.ChainStepConfig{{Provider: "big"}},
	}}, `{"complexity": 90, "hint": "deep", "domains": ["code"]}`)

	got := a.resolveRoute(context.Background(), ProcessMessageRequest{Message: "prove this terminates"})
	if got == nil || got.ChainLabel != "deep" {
		t.Fatalf("route = %+v; a complexity-only chain stopped routing", got)
	}
	if len(got.Judgment.Domains) != 0 {
		t.Errorf("domains = %v; nothing here could route on them", got.Judgment.Domains)
	}
	prompt := systemPromptSent(t, llm)
	if strings.Contains(prompt, "these exact tags") {
		t.Errorf("the prompt offers a list with nothing in it:\n%s", prompt)
	}
	if !strings.Contains(prompt, judgeNoDomainsWanted) {
		t.Errorf("the prompt does not say to leave domains empty:\n%s", prompt)
	}
}

// A reply of three invented tags and then the declared one is the shape
// the issue measured. Capping first would drop the tag the operator
// wrote a rule for, in favour of three that match nothing.
func TestTheVocabularyFilterRunsBeforeTheCap(t *testing.T) {
	t.Parallel()
	got := parsed(`{"domains": ["api-integration", "task-scheduling", "alert-systems", "legal"]}`, "legal")
	if !slices.Equal(got.Domains, []string{"legal"}) {
		t.Errorf("domains = %v; the cap threw away the only routable tag", got.Domains)
	}
}

// A model that ignores its list ignores it every turn, and the operator
// needs to hear that once: their chains declare subjects their preflight
// model will not produce, so subject routing is inert.
func TestAModelAnsweringOutsideItsListIsReportedOnce(t *testing.T) {
	t.Parallel()
	logs := &logCapture{}
	llm := &scriptedLLM{reply: `{"complexity": 40, "domains": ["api-integration"]}`}
	j := NewJudge(llm, "tiny", resolverOver(t, []string{"legal"}), 0,
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	for range 3 {
		j.Judge(context.Background(), "add a weather skill", "")
	}

	got := logs.String()
	if n := strings.Count(got, "answered outside the configured domains"); n != 1 {
		t.Errorf("the report appeared %d times over 3 turns; want once:\n%s", n, got)
	}
	for _, want := range []string{"api-integration", "legal"} {
		if !strings.Contains(got, want) {
			t.Errorf("the line names neither what was dropped nor what was configured (%q):\n%s", want, got)
		}
	}
}

// The ordinary deployment declares no domains at all. Dropping tags
// there is the design, not a signal, and a warning every turn about it
// would teach everyone to ignore the channel.
func TestWithNoDeclaredDomainsNothingIsReported(t *testing.T) {
	t.Parallel()
	logs := &logCapture{}
	llm := &scriptedLLM{reply: `{"complexity": 40, "domains": ["api-integration"]}`}
	j := NewJudge(llm, "tiny", nil, 0,
		slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	j.Judge(context.Background(), "add a weather skill", "")

	if logs.String() != "" {
		t.Errorf("a deployment that routes on nothing but complexity was warned:\n%s", logs.String())
	}
}
