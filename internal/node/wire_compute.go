package node

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/compute/research"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/modelsdev"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// wireCompute assembles the compute stack. The sequence is
// load-bearing, not incidental: the tool registries have to exist
// before any subsystem registers into them, capability discovery has
// to run before the modality tools pick a provider, and the Agent is
// last because it reads the finished registry plus the binary
// catalogue that tool registration produced. The per-subsystem helpers
// live in wire_compute_tools.go; this function is the running order.
func (n *Node) wireCompute() error {
	// hooks.Dispatcher from config.Hooks. NewDispatcher expects the
	// keyed-by-event map shape; the config's HooksConfig already
	// matches modulo a string→HookEvent conversion.
	hookEvents := make(map[types.HookEvent][]types.HookConfig, len(n.cfg.Hooks))
	for evtName, hs := range n.cfg.Hooks {
		hookEvents[types.HookEvent(evtName)] = hs
	}
	n.hooksDisp = hooks.NewDispatcher(hookEvents, n.log)

	// policy.Engine reads rules from the memory store. When policy
	// function is on another node, we skip engine wiring and the
	// Executor runs without policy gating (equivalent to default-
	// allow; deployments wanting strict policy must run the policy
	// function locally).
	if n.store != nil {
		n.policyEngine = policy.NewEngine(n.store, n.log)
	}

	// One health tracker per node, shared by the chat backup chain and
	// every modality chain. Shared on purpose: a provider that just
	// rejected the credential for read_image is the same endpoint
	// speak would reach, and rediscovering that per modality is the
	// waste this exists to remove.
	n.providerHealth = compute.NewProviderHealth()

	n.toolRegistry = compute.NewRegistry()
	n.executor = compute.NewExecutor(n.toolRegistry, n.policyEngine, n.hooksDisp, compute.ExecutorConfig{}, n.log)
	// One store, shared: the channel records "approve for this chat"
	// and the executor spends it. Two instances would mean the user
	// approves and is asked again anyway.
	n.approvals = compute.NewSessionApprovals()
	n.executor.SetSessionApprovals(n.approvals)

	builtins, err := n.wireStdlibTools()
	if err != nil {
		return err
	}

	embedder, err := n.wireEmbedder()
	if err != nil {
		return err
	}
	// Assigned HERE, once, rather than inside whichever branch built
	// it. The remote path used to set n.embedder itself, so when the
	// builtin path was added it returned a perfectly good provider and
	// left n.embedder nil — memory_search got it (that reads the return
	// value) while the episodic ingester and the context engine did not
	// (they read the field). The node logged "builtin embedding model
	// ready" and then wrote no vectors at all, which is a failure with
	// no error in it anywhere.
	n.embedder = embedder

	// binariesProvider comes back out of tool registration so the Agent
	// (constructed below) can advertise the operator's [[binary]]
	// catalogue in the system prompt every turn. Nil when no [[binary]]
	// is declared; nil-checked at the agent call site.
	binariesProvider, err := n.registerAgentTools(builtins, embedder)
	if err != nil {
		return err
	}

	// Wire the skill registry's PolicySink so skill-bundled policy.d/
	// subtrees apply to the tool registry during scan. Order matters:
	// skills scanned BEFORE operator's policy.d load means
	// operator-authored policies win on overlap (SANDBOX.md §
	// "Skill-bundled policies" step 2).
	if n.skillRegistry != nil {
		n.skillRegistry.SetPolicySink(n.toolRegistry)
	}

	if err := n.wireResolver(); err != nil {
		return err
	}

	clientsByLabel, err := n.wireLLMProviders()
	if err != nil {
		return err
	}

	if err := n.wireRoleMap(clientsByLabel); err != nil {
		return err
	}

	// Council registration happens HERE, after wireLLMProviders has
	// populated the registry, and not with the other tools above.
	//
	// It used to sit with them, guarded on n.providerRegistry != nil —
	// which is assigned 200 lines further down, inside
	// wireLLMProviders. wireCompute runs once, the field starts nil,
	// and nothing else assigns it, so the guard was never true and
	// list_providers and council_review have never registered on any
	// node. The guard read like a capability check and was really a
	// read of a variable that did not exist yet.
	// After the self-taught stage, which runs earlier in nodeWireStages
	// and is what sets n.selfTaught. Registering before it would find
	// nil and skip the tool on every node that has the store.
	if err := n.wireLearnedTools(builtins); err != nil {
		return err
	}
	if err := n.wireCouncilTools(builtins); err != nil {
		return err
	}

	if err := n.wireAgent(binariesProvider); err != nil {
		return err
	}

	// When both the agent and scheduler are present on this node,
	// register the built-in "agent:turn" handler so operators can
	// schedule tasks + commitments that dispatch through the agent
	// loop without writing custom handler code.
	if n.agent != nil && n.scheduler != nil {
		n.registerAgentTurnHandlers()
		// research:run handler also lives here (not in wireScheduler)
		// because it needs the agent to drive the research loop and
		// the agent isn't constructed until this stage.
		n.registerResearchHandler()
		// Dream's summarizer likewise: it resolves the summariser
		// ROLE, and the role map is built in this stage. Attaching it
		// from wireScheduler — where the dream handler is registered
		// — would find a nil map and silently leave consolidation
		// switched off, which is indistinguishable from the state it
		// has been in since it was written.
		n.attachDreamSummarizer()
	}

	n.log.Info("compute stack wired",
		"has_policy_engine", n.policyEngine != nil,
		"providers", len(n.cfg.Compute.Providers),
		"chains", len(n.cfg.Compute.Chains),
		"has_agent", n.agent != nil,
	)
	return nil
}

// wireResolver builds the provider/chain resolver.
//
// The resolver VALIDATES chains and then nothing routes through it.
// `Resolver.Resolve` has no callers: the turn path is the provider
// backup chain (ProviderRegistry.Chain), which knows about `backup`
// links and nothing about triggers, multi-step chains or per-chain
// trust floors. So `[[compute.chains]]` is parsed, checked for
// coherence, and inert.
//
// Kept, rather than deleted, because the validation is worth having
// the day the routing lands and deleting it would silently accept
// broken chains in the meantime. But an operator who writes a chain
// and sees it accepted is entitled to think it does something, so
// this says otherwise, loudly, once.
func (n *Node) wireResolver() error {
	// Before anything is constructed, and fatal. A dev source that is
	// configured but not gated is a node that would either silently
	// skip an override the operator is developing against, or run an
	// unsigned one in production without saying so.
	if err := n.checkDevSource(); err != nil {
		return err
	}
	if len(n.cfg.Compute.Providers) == 0 {
		return nil
	}
	r, err := compute.NewResolver(&n.cfg.Compute)
	if err != nil {
		return fmt.Errorf("resolver: %w", err)
	}
	n.resolver = r
	return nil
}

// newJudge builds the routing signal from the preflight role.
//
// The preflight role already existed for this shape of work — a small,
// fast model classifying ahead of the main turn — and falls back to
// main when unset, so a deployment that never configured one still
// routes, just at the main model's price.
func (n *Node) newJudge() *compute.Judge {
	if n.roleMap == nil {
		return nil
	}
	label := n.cfg.Compute.Roles.Preflight
	if label == "" {
		label = n.primaryProviderLabel()
	}
	var model string
	for _, p := range n.cfg.Compute.Providers {
		if p.Label == label {
			model = p.Model
			break
		}
	}
	return compute.NewJudge(n.roleMap.For(compute.RolePreflight), model, n.log)
}

// wireLLMProviders builds the LLM clients: injection wins for the main
// slot; else build a client per configured [[compute.providers]]
// entry. The returned label→client map is what wireRoleMap resolves
// the RoleMap against.
func (n *Node) wireLLMProviders() (map[string]compute.LLMProvider, error) {
	clientsByLabel := map[string]compute.LLMProvider{}
	switch {
	case n.cfg.LLMProvider != nil:
		n.llmProvider = n.cfg.LLMProvider
		clientsByLabel["main"] = n.cfg.LLMProvider
	case len(n.cfg.Compute.Providers) > 0:
		n.providerRegistry = compute.NewProviderRegistry()
		for i, p := range n.cfg.Compute.Providers {
			// Trust-tier guard on every provider — a misconfigured
			// secondary shouldn't slip past the Soul's floor just
			// because it's not the main turn.
			if err := soul.ValidateProviderTier(n.Soul(), soul.ProviderTrustTier{
				Label:     p.Label,
				TrustTier: p.TrustTier,
			}); err != nil {
				return nil, fmt.Errorf("provider %q: %w", p.Label, err)
			}
			apiKey, err := n.resolveAPIKey(p.APIKeyRef)
			if err != nil {
				return nil, fmt.Errorf("api key for provider %q: %w", p.Label, err)
			}
			client, err := n.drivers().Chat(p.Driver, compute.ChatDriverConfig{
				Endpoint:    p.Endpoint,
				Model:       p.Model,
				Credential:  credentialFor(p, apiKey),
				ServerTools: serverToolsFromConfig(p.ServerTools),
				Logger:      n.log,
			})
			if err != nil {
				return nil, fmt.Errorf("provider %q: %w", p.Label, err)
			}
			clientsByLabel[p.Label] = client
			n.providerRegistry.Register(compute.ProviderEntry{
				Label:        p.Label,
				TrustTier:    p.TrustTier,
				Capabilities: p.Capabilities,
				Backup:       p.Backup,
				Client:       client,
				Model:        p.Model,
				Pricing:      p.Pricing,
			})
			if i == 0 {
				n.llmProvider = client
			}
		}
	}
	return clientsByLabel, nil
}

// defaultProviderLabel names the provider used when compute.roles.main
// is unset — the first declared one, matching wireLLMProviders.
//
// Named rather than left blank, because "no label" and "the first one"
// read identically to anyone asking which provider serves main, and
// the first-provider default is precisely the case where they cannot
// tell from config alone.
func (n *Node) defaultProviderLabel() string {
	for _, p := range n.cfg.Compute.Providers {
		if p.Label != "" {
			return p.Label
		}
	}
	return ""
}

// wireRoleMap resolves [compute.roles] against the built clients. An
// explicit role map from config overrides fallback picks.
func (n *Node) wireRoleMap(clientsByLabel map[string]compute.LLMProvider) error {
	n.roleMap = nil
	if n.llmProvider == nil {
		return nil
	}
	roleAssignments := map[compute.Role]compute.LLMProvider{}
	roleLabels := map[compute.Role]string{}
	pickRole := func(role compute.Role, label string) error {
		if label == "" {
			return nil
		}
		c, ok := clientsByLabel[label]
		if !ok {
			return fmt.Errorf("compute.roles.%s: unknown provider label %q", role, label)
		}
		roleAssignments[role] = c
		roleLabels[role] = label
		return nil
	}
	if err := pickRole(compute.RoleMain, n.cfg.Compute.Roles.Main); err != nil {
		return err
	}
	if err := pickRole(compute.RolePreflight, n.cfg.Compute.Roles.Preflight); err != nil {
		return err
	}
	if err := pickRole(compute.RoleReranker, n.cfg.Compute.Roles.Reranker); err != nil {
		return err
	}
	if err := pickRole(compute.RoleSummariser, n.cfg.Compute.Roles.Summariser); err != nil {
		return err
	}
	// If compute.roles.main was set, it overrides first-provider.
	main := n.llmProvider
	mainLabel := n.defaultProviderLabel()
	if override, ok := roleAssignments[compute.RoleMain]; ok {
		main = override
		n.llmProvider = override
		mainLabel = roleLabels[compute.RoleMain]
	}
	rm, err := compute.NewRoleMapWithLabels(main, roleAssignments, mainLabel, roleLabels)
	if err != nil {
		return fmt.Errorf("role map: %w", err)
	}
	n.roleMap = rm
	return nil
}

// wireAgent constructs the Agent. Only constructable with a non-nil
// Provider: a Compute-enabled node with no providers gets n.agent=nil
// — the REST handler surfaces "provider not configured" at message
// time rather than blocking boot.
func (n *Node) wireAgent(binariesProvider func() []promptgen.BinaryInfo) error {
	if n.llmProvider == nil {
		return nil
	}
	var episodicIngester compute.EpisodicIngester
	if n.raft != nil {
		var memEmbedder memory.Embedder
		if n.embedder != nil {
			memEmbedder = n.embedder
		}
		ingester, err := memory.NewEpisodicIngester(n.raft, 0, memEmbedder)
		if err != nil {
			return fmt.Errorf("episodic ingester: %w", err)
		}
		episodicIngester = &episodicIngesterAdapter{inner: ingester}
	}
	primaryLabel := n.primaryProviderLabel()
	a, err := compute.NewAgent(compute.AgentConfig{
		Provider:     n.llmProvider,
		PrimaryLabel: primaryLabel,
		Traces:       n.traces,
		// So the assistant can answer truthfully when asked what it
		// is configured to do, rather than from the shape of its tool
		// list.
		SelfLearningMode: n.cfg.SelfLearningMode,
		Providers:        n.providerRegistry,
		Resolver:         n.resolver,
		Judge:            n.newJudge(),
		Health:           n.providerHealth,
		Executor:         n.executor,
		Registry:         n.toolRegistry,
		Soul: func() *types.SoulConfig {
			s := n.Soul()
			if s == nil {
				return nil
			}
			return &s.Config
		},
		EpisodicIngester: episodicIngester,
		Roles:            n.roleMap,
		Identity:         n.identityResolver(),
		ContextEngine: compute.NewContextEngine(compute.ContextEngineConfig{
			Store:      n.store,
			Embedder:   n.embedder,
			CrossOwner: n.crossOwnerAuthz(),
			Logger:     n.log,
		}),
		Skills:            skillDispatcherOrNil(n.skillAdapter),
		SkillsProvider:    n.skillIndexProvider(),
		PinnedProvider:    n.pinnedProvider(),
		ProposalsProvider: n.proposalsProvider(),
		// Populated after this stage by wire-review-fork, which needs
		// the RoleMap built here. Set on the agent below rather than
		// passed in, for that ordering.
		TimezoneResolver: n.resolveUserTimezone,
		BinariesProvider: binariesProvider,
		ContextBudget:    contextBudgetFromConfig(n.cfg.Compute.Context),
		Logger:           n.log,
	})
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	n.agent = a
	return nil
}

// AgentTurnHandlerRef is the well-known HandlerRef that dispatches a
// scheduled task or commitment as an agent turn. Operators who want
// "every morning run the check-in skill" configure a task with this
// ref and a Params["prompt"].
const AgentTurnHandlerRef = "agent:turn"

// DreamHandlerRef is the well-known HandlerRef for the memory
// Dream/REM consolidation pass. Every node's scheduler races to
// claim a scheduled_tasks entry with this ref, and the winner runs
// one Dream pass. DreamRunner itself is leader-only-gated so a
// claim winner on a non-leader soft-skips.
//
// Handler-ref namespaces are semantic prefixes, not implementation
// categories: "agent:" dispatches through the LLM agent loop,
// "memory:" dispatches to a memory-layer Go-native operation.
// Renamed from the earlier "memory:dream" to avoid implying this is
// a Phase 8 on-disk skill (it isn't — there's no manifest, no
// subprocess; it's an internal Go operation).
const DreamHandlerRef = "memory:dream"

// SessionPruneHandlerRef is the well-known HandlerRef for the
// retention=session hard-prune pass. Like dream, every node's
// scheduler races to claim the task; the pruner itself soft-skips
// on non-leaders so duplicate claims are safe.
const SessionPruneHandlerRef = "memory:session-prune"

// attachDreamSummarizer gives Dream the thing that lets it
// consolidate, on the SUMMARISER role.
//
// Without it Dream scored, pruned and digested and never once
// consolidated — the step is gated on a Summarizer and nothing
// outside tests ever set one. On a cluster with 157 episodic records
// that produced "candidates=10 consolidated=0" every night, which
// reads as a working pass finding nothing worth merging.
//
// Left nil when there is no role map, which is a compute-less node:
// Dream keeps scoring and pruning, as it always did.
func (n *Node) attachDreamSummarizer() {
	if n.roleMap == nil || n.memorySvc == nil {
		return
	}
	runner := n.memorySvc.DreamRunner()
	if runner == nil {
		return
	}
	label := n.cfg.Compute.Roles.Summariser
	if label == "" {
		label = n.primaryProviderLabel()
	}
	var model string
	for _, p := range n.cfg.Compute.Providers {
		if p.Label == label {
			model = p.Model
			break
		}
	}
	s := compute.NewDreamSummarizer(n.roleMap.For(compute.RoleSummariser), model, n.log)
	if s == nil {
		return
	}
	runner.SetSummarizer(s)
	n.log.Info("dream: consolidation enabled", "provider", label, "model", model)
}

// registerDreamHandler wires DreamRunner into the scheduler so an
// operator's `handler = "memory:dream"` ScheduledTask actually fires
// the Dream pass. Called from node.New when both memorySvc and
// scheduler are present on this node (i.e. any Raft-hosting node).
func (n *Node) registerDreamHandler() {
	if n.memorySvc == nil || n.scheduler == nil {
		return
	}
	runner := n.memorySvc.DreamRunner()
	if runner == nil {
		return
	}
	_ = n.scheduler.Handlers().RegisterTask(DreamHandlerRef,
		func(ctx context.Context, _ *lobslawv1.ScheduledTaskRecord) error {
			result, err := runner.Run(ctx)
			if err != nil {
				return fmt.Errorf("dream: %w", err)
			}
			if result == nil {
				// Non-leader soft-skip — runner already logged.
				return nil
			}
			n.log.Info("scheduler: dream pass completed",
				"candidates", result.Candidates,
				"consolidated", result.Consolidated,
				"pruned", result.Pruned,
			)
			return nil
		})
}

// llmEndpoint is a fully-resolved LLM endpoint — endpoint URL +
// model + already-resolved API key + wire format. Modality blocks
// (vision, audio, future STT) all reduce to one of these.
//
// matchedCap is the capability tag that selected this endpoint
// (e.g. "audio-transcription" vs "audio-multimodal"); modality
// dispatch can switch on it to pick the right wire shape.
type llmEndpoint struct {
	endpoint string
	model    string
	apiKey   string
	format   string
	via      string // "override:<label>", "capability:<label>"
	// label is the provider's [[compute.providers]] label, used as the
	// health-tracking key. Parsed out of `via` would work and would
	// break the first time the via format changed.
	label string
	// trustTier is the provider's declared tier, carried through so
	// the modality failover chain can honour the soul floor. Read off
	// the provider config here rather than looked up again at
	// registration, because the two lookups could disagree and the
	// disagreement would be silent.
	trustTier  types.TrustTier
	matchedCap string // empty if override; else the capability that matched
	// driver is the provider's declared wire protocol. Empty means the
	// modality's default, which is the OpenAI-compatible shape
	// everywhere it has one.
	driver string
	// pricing is what the provider charges, carried through for the
	// same reason trustTier is: looking it up again at registration
	// could disagree with what was read here, and the disagreement
	// would be silent.
	pricing types.ProviderPricing
}

// findProvider scans cfg.Compute.Providers for one matching label.
// Returns nil when not found — caller decides whether to fall back
// to inline or surface an error.
func (n *Node) findProvider(label string) *config.ProviderConfig {
	if label == "" {
		return nil
	}
	for i := range n.cfg.Compute.Providers {
		p := &n.cfg.Compute.Providers[i]
		if p.Label == label {
			return p
		}
	}
	return nil
}

// resolveModalityEndpoints discovers the provider chain for the given
// modality, in the order it should be tried. Resolution order:
//
//  1. If override.Provider is set, pin to that label and STOP. An
//     explicit override is an operator saying "use this one"; quietly
//     falling back to a provider they did not name would spend money
//     at a cost and trust tier they did not choose. Operators who want
//     a chain express it with capability tags instead.
//  2. Otherwise scan [[compute.providers]] for matching capabilities
//     (anyOf), highest Priority first — and keep ALL of them. That
//     ordering is what SelectByCapability has always returned; the
//     resolver used to take the head and discard the tail, which is
//     why a modality had no failover while chat did.
//  3. Empty when nothing matches — the caller omits the builtin and
//     the agent honestly reports it can't.
func (n *Node) resolveModalityEndpoints(modality, overrideLabel string, anyOf ...string) []*llmEndpoint {
	if overrideLabel != "" {
		p := n.findProvider(overrideLabel)
		if p == nil {
			n.log.Warn("compute: "+modality+" override references unknown provider; skipping",
				"label", overrideLabel)
			return nil
		}
		ep := n.endpointFromProvider(modality, *p, "override:"+overrideLabel)
		if ep == nil {
			return nil
		}
		ep.label = overrideLabel
		return []*llmEndpoint{ep}
	}
	var out []*llmEndpoint
	seen := map[string]bool{}
	for _, want := range anyOf {
		for _, p := range compute.SelectByCapability(n.cfg.Compute.Providers, want) {
			// A provider tagged with two of the requested capabilities
			// would otherwise appear twice and be retried against itself.
			if seen[p.Label] {
				continue
			}
			ep := n.endpointFromProvider(modality, p, "capability:"+p.Label)
			if ep == nil {
				continue // key unresolvable; already warned
			}
			seen[p.Label] = true
			ep.label = p.Label
			ep.matchedCap = want
			out = append(out, ep)
		}
	}
	return out
}

func (n *Node) endpointFromProvider(modality string, p config.ProviderConfig, via string) *llmEndpoint {
	key, err := n.resolveAPIKey(p.APIKeyRef)
	if err != nil || key == "" {
		n.log.Warn("compute: "+modality+" provider key not resolvable; skipping",
			"label", p.Label, "err", err)
		return nil
	}
	format := p.Format
	if format == "" {
		format = "openai"
	}
	return &llmEndpoint{
		endpoint: p.Endpoint,
		model:    p.Model,
		apiKey:   key,
		format:   format,
		via:      via,
		driver:   p.Driver,
		// Both read off the same provider config, in one place. The
		// label was set by two callers before; a tier set separately
		// could end up describing a different provider than the label
		// it travels with, and nothing would catch it.
		label:     p.Label,
		trustTier: p.TrustTier,
		// Unset pricing is not an error: a plan-billed provider has no
		// marginal rate to quote, and a modality that refused to run
		// without one would be refusing over the accounting rather
		// than the work. The quantity is recorded either way, so
		// consumption stays visible where the marginal cost is nil.
		pricing: p.Pricing,
	}
}

func (n *Node) resolveVisionEndpoints() []*llmEndpoint {
	return n.resolveModalityEndpoints("vision", n.cfg.Compute.Vision.Provider, compute.CapabilityVision)
}

func (n *Node) resolveAudioEndpoints() []*llmEndpoint {
	return n.resolveModalityEndpoints("audio", n.cfg.Compute.Audio.Provider,
		compute.CapabilityAudioTranscribe, compute.CapabilityAudioMultimodal)
}

func (n *Node) resolvePDFEndpoints() []*llmEndpoint {
	return n.resolveModalityEndpoints("pdf", n.cfg.Compute.PDF.Provider, compute.CapabilityPDF)
}

// applyModelsDevAutoCapabilities mutates n.cfg.Compute.Providers in
// place, merging discovered capabilities for entries with
// auto_capabilities = true. No-op when no provider opts in. The
// catalog fetch is shared across all opted-in providers (single
// HTTP call, 24h disk cache). Failures degrade gracefully — operator
// keeps whatever they declared.
func (n *Node) applyModelsDevAutoCapabilities(ctx context.Context) {
	wantsDiscovery := false
	for _, p := range n.cfg.Compute.Providers {
		// One fetch serves both. They are separate opt-ins because
		// they fail differently, not because they need separate
		// catalogues.
		if p.AutoCapabilities || p.AutoPricing {
			wantsDiscovery = true
			break
		}
	}
	if !wantsDiscovery {
		return
	}

	fetcher := modelsdev.NewFetcher()
	if n.cfg.DataDir != "" {
		fetcher.CacheDir = filepath.Join(n.cfg.DataDir, "cache")
	}
	cat, err := fetcher.Fetch(ctx)
	if err != nil {
		n.log.Warn("modelsdev: fetch failed; auto_capabilities providers fall back to declared caps only",
			"err", err)
		if cat == nil {
			return
		}
		// err non-nil but cat returned → stale-cache path. Continue.
		n.log.Info("modelsdev: using stale cache")
	}

	for i := range n.cfg.Compute.Providers {
		p := &n.cfg.Compute.Providers[i]
		if !p.AutoCapabilities && !p.AutoPricing {
			continue
		}
		// The endpoint identifies WHICH catalog provider this is, and
		// that entry is better evidence than a vote across providers
		// nobody here uses.
		//
		// The consensus rule is an intersection, and it was reached
		// FIRST — so qwen3.7-plus, listed by twelve providers, lost
		// its image input because two of them (aihubmix, hyper) have
		// it wrong. The ten that agree include alibaba-token-plan,
		// which is the endpoint actually configured. Discovery then
		// reported "no new capabilities" and read_image did not
		// register, on a provider whose own catalogue entry says it
		// takes images.
		//
		// Consensus still covers the case it was written for: when
		// the endpoint matches no catalogue provider, a vote across
		// everything carrying that model name is the best available
		// evidence, and its conservatism is right there.
		hint := p.Endpoint
		var matches []modelsdev.Model
		if hinted, ok := cat.LookupProvider(hint, p.Model); ok {
			matches = []modelsdev.Model{hinted}
		} else {
			matches = cat.LookupAll(p.Model)
		}
		if len(matches) == 0 {
			n.log.Info("modelsdev: model not found in catalog; using declared caps only",
				"label", p.Label, "model", p.Model)
			continue
		}
		if p.AutoPricing {
			n.applyCatalogPricing(p, matches)
		}
		if !p.AutoCapabilities {
			continue
		}
		discovered := compute.CapabilitiesFromConsensus(matches)
		merged := compute.MergeCapabilities(p.Capabilities, discovered)
		if len(merged) == len(p.Capabilities) {
			n.log.Debug("modelsdev: no new capabilities discovered",
				"label", p.Label, "model", p.Model, "matches", len(matches))
			continue
		}
		added := diffCapabilities(merged, p.Capabilities)
		n.log.Info("modelsdev: capabilities augmented from catalog",
			"label", p.Label, "model", p.Model,
			"matches", len(matches),
			"added", added, "all", merged)
		p.Capabilities = merged
	}

	n.warnUnsupportedCapabilities(cat)
}

// applyCatalogPricing fills a rate card from the catalogue.
//
// Only from the PROVIDER-SPECIFIC entry. The consensus rule takes an
// intersection, which is a defensible answer for "can it do X" and a
// meaningless one for "what does it cost" — the cheapest listing
// across twelve resellers is not this endpoint's price. When the
// endpoint matches no catalogue provider there is no price to fill,
// and reporting nothing beats reporting somebody else's.
func (n *Node) applyCatalogPricing(p *config.ProviderConfig, matches []modelsdev.Model) {
	if len(matches) != 1 {
		n.log.Debug("modelsdev: no provider-specific entry, leaving pricing alone",
			"label", p.Label, "model", p.Model, "matches", len(matches))
		return
	}
	discovered := compute.PricingFromModel(matches[0])
	merged := compute.MergePricing(p.Pricing, discovered)
	// Nothing to say when the declared card won, or when the
	// catalogue's rate is the zero one a plan-billed provider has.
	if compute.PricingIsZero(merged) || !compute.PricingIsZero(p.Pricing) {
		return
	}
	p.Pricing = merged
	n.log.Info("modelsdev: pricing taken from catalog",
		"label", p.Label, "model", p.Model,
		"input_per_1k", merged.InputUSDPer1K,
		"output_per_1k", merged.OutputUSDPer1K,
		"cached_per_1k", merged.CachedUSDPer1K)
}

// warnUnsupportedCapabilities tells an operator when a provider claims
// something its model cannot do.
//
// WARN, NOT REFUSE. The catalogue is third-party data that can be
// stale or wrong, and a self-hosted model may genuinely do something
// models.dev has never heard of. Refusing to boot on somebody else's
// data about somebody else's model would take a cluster down over a
// missing catalogue entry.
//
// EVERY provider, not only the ones that opted into discovery. A
// declared capability is a claim about the world whoever wrote it, and
// the operator most likely to have got it wrong is the one who typed
// the list by hand.
//
// The catalogue is only in hand when something opted into
// auto_capabilities, and this deliberately does not fetch it
// otherwise: a mandatory boot-time HTTP call would break an
// air-gapped node to deliver a warning.
func (n *Node) warnUnsupportedCapabilities(cat modelsdev.Catalog) {
	if len(cat) == 0 {
		return
	}
	for i := range n.cfg.Compute.Providers {
		p := &n.cfg.Compute.Providers[i]
		matches := cat.LookupAll(p.Model)
		if len(matches) == 0 {
			if hinted, ok := cat.Lookup(p.Endpoint, p.Model); ok {
				matches = []modelsdev.Model{hinted}
			}
		}
		unsupported := compute.UnsupportedCapabilities(p.Capabilities, matches)
		if len(unsupported) == 0 {
			continue
		}
		// The MODEL is named as well as the label, because the usual
		// cause is a copied provider block whose model was changed and
		// whose capability list was not.
		n.log.Warn("compute: provider declares capabilities its model is not listed as supporting; "+
			"calls routed there on these capabilities will probably fail",
			"label", p.Label, "model", p.Model,
			"unsupported", unsupported, "catalog_entries", len(matches))
	}
}

// diffCapabilities returns the items in `merged` not present in
// `original`. Used purely for the INFO log so operators can see
// what auto-discovery added vs. what was already declared.
func diffCapabilities(merged, original []string) []string {
	have := make(map[string]struct{}, len(original))
	for _, c := range original {
		have[c] = struct{}{}
	}
	var added []string
	for _, c := range merged {
		if _, ok := have[c]; !ok {
			added = append(added, c)
		}
	}
	return added
}

// registerSessionPruneHandler wires SessionPruner into the scheduler
// so the auto-seeded `memory:session-prune` task runs on the leader.
// Configures the pruner from cfg.MemorySession.MaxAge before wiring
// so operator overrides take effect.
func (n *Node) registerSessionPruneHandler() {
	if n.memorySvc == nil || n.scheduler == nil {
		return
	}
	if maxAge := n.cfg.MemorySession.MaxAge; maxAge > 0 {
		n.memorySvc.ConfigureSessionPruner(maxAge)
	}
	pruner := n.memorySvc.SessionPruner()
	if pruner == nil {
		return
	}
	_ = n.scheduler.Handlers().RegisterTask(SessionPruneHandlerRef,
		func(ctx context.Context, _ *lobslawv1.ScheduledTaskRecord) error {
			result, err := pruner.Run(ctx)
			if err != nil {
				return fmt.Errorf("session-prune: %w", err)
			}
			if result == nil {
				return nil
			}
			n.log.Info("scheduler: session prune completed",
				"episodic_pruned", result.EpisodicPruned,
				"vector_pruned", result.VectorPruned,
			)
			return nil
		})
}

// seedSessionPruneTask installs a recurring memory:session-prune
// task under "lobslaw-builtin-session-prune" if not already present.
// Default cadence: hourly. Operator opt-out via [memory.session]
// enabled = false. Idempotent — schedule changes require deleting
// the seeded task first (next boot re-seeds).
func (n *Node) seedSessionPruneTask(ctx context.Context) error {
	if n.raft == nil || n.store == nil || n.scheduler == nil || n.memorySvc == nil {
		return nil
	}
	if !n.raft.IsLeader() {
		return nil
	}
	if n.cfg.MemorySession.Enabled != nil && !*n.cfg.MemorySession.Enabled {
		return nil
	}
	const taskID = "lobslaw-builtin-session-prune"
	if _, err := n.store.Get(memory.BucketScheduledTasks, taskID); err == nil {
		return nil
	}
	schedule := strings.TrimSpace(n.cfg.MemorySession.Schedule)
	if schedule == "" {
		schedule = "@hourly"
	}
	task := &lobslawv1.ScheduledTaskRecord{
		Id:         taskID,
		Name:       "memory.session-prune (builtin)",
		Schedule:   schedule,
		HandlerRef: SessionPruneHandlerRef,
		Enabled:    true,
		CreatedAt:  timestamppb.Now(),
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      taskID,
		Payload: &lobslawv1.LogEntry_ScheduledTask{ScheduledTask: task},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal session prune task: %w", err)
	}
	if _, err := n.raft.Apply(data, 5*time.Second); err != nil {
		return fmt.Errorf("apply session prune task: %w", err)
	}
	n.log.Info("memory: seeded session prune task", "id", taskID, "schedule", schedule)
	return nil
}

// registerAgentTurnHandlers installs the default task + commitment
// handlers that drive compute.Agent.RunToolCallLoop with the
// scheduler-originated request. Intended to be called once during
// boot; subsequent calls overwrite the prior registration (fine —
// RegisterTask/RegisterCommitment are last-write-wins).
func (n *Node) registerAgentTurnHandlers() {
	_ = n.scheduler.Handlers().RegisterTask(AgentTurnHandlerRef, n.runTaskAsAgentTurn)
	_ = n.scheduler.Handlers().RegisterCommitment(AgentTurnHandlerRef, n.runCommitmentAsAgentTurn)

	// Idempotent, unlike every other commitment handler here. Polling a
	// provider for a job it is already running costs one cheap request
	// if it happens twice; losing the only poll orphans work that is
	// already being billed. See scheduler.Idempotent.
	_ = n.scheduler.Handlers().RegisterCommitment(
		GenerationPollHandlerRef, n.runGenerationPoll, scheduler.Idempotent())
}

// researchIDEntropy is a process-wide ULID monotonic source for
// research record IDs. Each adapter call hits this.

// registerResearchHandler wires the research:run commitment
// handler that drives the deep-research pipeline. Only registered
// when the agent + memory + tool registry are all present (i.e.
// memory-function nodes that also host compute). Worker tools come
// from the live registry at fire time so MCP-supplied tools are
// usable by research workers automatically.
func (n *Node) registerResearchHandler() {
	if n.agent == nil || n.memorySvc == nil || n.scheduler == nil || n.toolRegistry == nil {
		return
	}
	_ = n.scheduler.Handlers().RegisterCommitment(compute.ResearchHandlerRef, n.runResearchCommitment)
}

// runResearchCommitment unpacks the commitment params + dispatches
// to the research.Coordinator. Tool list captured at fire time so
// any MCP tools registered after boot are picked up.
func (n *Node) runResearchCommitment(ctx context.Context, c *lobslawv1.AgentCommitment) error {
	question := c.Params["question"]
	if question == "" {
		return fmt.Errorf("research: commitment %q missing question", c.Id)
	}
	depth := 3
	if d := c.Params["depth"]; d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			depth = n
		}
	}
	tools := buildResearchToolList(n.toolRegistry)
	coord := research.NewCoordinator(research.Config{
		Agent:       n.agent,
		LLMProvider: n.llmProvider,
		Memory:      &researchMemoryAdapter{svc: n.memorySvc},
		Notify:      &researchNotifyAdapter{tg: n.telegramHandler, log: n.log},
		WorkerTools: tools,
		Logger:      n.log,
	})
	_, err := coord.Run(ctx, research.Request{
		TaskID:            c.Id,
		Question:          question,
		Depth:             depth,
		OriginatorChannel: c.Params["originator_channel"],
		OriginatorChatID:  c.Params["originator_chat_id"],
		Claims:            schedulerClaims(),
	})
	return err
}

// buildResearchToolList scopes the tool list to read-oriented
// builtins + every MCP tool. Excludes write_file/edit_file/
// shell_command — research workers should fetch + summarise, not
// mutate the workspace. Future: an explicit `[research] allow_tools`
// config to override this.
func buildResearchToolList(reg *compute.Registry) []compute.Tool {
	allowed := map[string]bool{
		"web_search":     true,
		"fetch_url":      true,
		"memory_search":  true,
		"memory_write":   true,
		"list_providers": true,
		"council_review": true,
	}
	defs := reg.List()
	out := make([]compute.Tool, 0, len(defs))
	for _, d := range defs {
		// Allow all MCP-namespaced tools (have a dot in the name).
		if strings.Contains(d.Name, ".") || allowed[d.Name] {
			out = append(out, compute.Tool{
				Name:        d.Name,
				Description: d.Description,
				Parameters:  d.ParametersSchema,
			})
		}
	}
	return out
}

// researchMemoryAdapter satisfies research.MemoryWriter using the
// node's memory.Service. Records get a fresh ULID + episodic
// retention; tags flow through verbatim.
type researchMemoryAdapter struct{ svc *memory.Service }

func (a *researchMemoryAdapter) WriteEpisodic(ctx context.Context, content string, tags []string) (string, error) {
	id := "research-" + ids.New()
	rec := &lobslawv1.EpisodicRecord{
		Id:         id,
		Event:      "research-finding",
		Context:    content,
		Tags:       tags,
		Importance: 7, // research output ranks above default-5
	}
	resp, err := a.svc.EpisodicAdd(ctx, &lobslawv1.EpisodicAddRequest{Record: rec})
	if err != nil {
		return "", err
	}
	return resp.Id, nil
}

// researchNotifyAdapter satisfies research.Notifier. Today only
// Telegram is wired; REST/webhook notification follow when those
// channels gain proactive-message helpers.
type researchNotifyAdapter struct {
	tg  *gateway.TelegramHandler
	log *slog.Logger
}

func (a *researchNotifyAdapter) Notify(_ context.Context, channel, channelID, body string) error {
	if channel != "telegram" || a.tg == nil || channelID == "" {
		a.log.Warn("research: notification skipped (channel unsupported / not wired)",
			"channel", channel, "channel_id_set", channelID != "")
		return nil
	}
	chatID, err := strconv.ParseInt(channelID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid telegram chat_id %q: %w", channelID, err)
	}
	return a.tg.Send(chatID, body)
}

func schedulerClaims() *types.Claims {
	return &types.Claims{UserID: "scheduler", Scope: "default"}
}

// runTaskAsAgentTurn dispatches a scheduled task's Params["prompt"]
// through the agent loop with synthetic "scheduler" claims and a
// fresh TurnBudget. A missing prompt is a config error — we log +
// return instead of running an empty turn (which would waste a
// provider call).
func (n *Node) runTaskAsAgentTurn(ctx context.Context, task *lobslawv1.ScheduledTaskRecord) error {
	prompt := task.Params["prompt"]
	if prompt == "" {
		return fmt.Errorf("scheduled task %q: params.prompt missing", task.Id)
	}
	budget, err := compute.NewTurnBudget(compute.FromComputeConfig(n.cfg.Compute))
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	req := compute.ProcessMessageRequest{
		Message:   prompt,
		Claims:    n.schedulerClaims(task.CreatedBy),
		TurnID:    fmt.Sprintf("task-%s-%d", task.Id, time.Now().UnixNano()),
		Budget:    budget,
		Channel:   task.Params["channel"],
		ChannelID: task.Params["chat_id"],
	}
	resp, err := n.agent.RunToolCallLoop(ctx, req)
	if err != nil {
		return fmt.Errorf("agent loop: %w", err)
	}
	n.log.Info("scheduler: agent task completed",
		"task_id", task.Id,
		"turn_id", req.TurnID,
		"tool_calls", len(resp.ToolCalls),
		"needs_confirm", resp.NeedsConfirmation,
	)
	return nil
}

// runCommitmentAsAgentTurn is the one-shot equivalent. Prefers
// Params["prompt"]; falls back to Reason (so commitments created
// via natural-language "remind me to check the oven in 2 hours"
// round-trip the description as the prompt body).
func (n *Node) runCommitmentAsAgentTurn(ctx context.Context, c *lobslawv1.AgentCommitment) error {
	prompt := c.Params["prompt"]
	if prompt == "" {
		prompt = c.Reason
	}
	if prompt == "" {
		return fmt.Errorf("commitment %q: no prompt or reason", c.Id)
	}
	budget, err := compute.NewTurnBudget(compute.FromComputeConfig(n.cfg.Compute))
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	req := compute.ProcessMessageRequest{
		Message:   prompt,
		Claims:    n.schedulerClaims(c.CreatedFor),
		TurnID:    fmt.Sprintf("commitment-%s-%d", c.Id, time.Now().UnixNano()),
		Budget:    budget,
		Channel:   c.Params["channel"],
		ChannelID: c.Params["chat_id"],
	}
	resp, err := n.agent.RunToolCallLoop(ctx, req)
	if err != nil {
		return fmt.Errorf("agent loop: %w", err)
	}
	n.log.Info("scheduler: agent commitment completed",
		"commitment_id", c.Id,
		"turn_id", req.TurnID,
		"tool_calls", len(resp.ToolCalls),
		"needs_confirm", resp.NeedsConfirmation,
	)
	return nil
}

// schedulerClaims builds the synthetic claims attached to a
// scheduler-originated turn. UserID traces back to whoever created
// the task/commitment so audit can distinguish "alice scheduled
// this" from "bob did." Scope defaults to "scheduler" so policies
// can gate what scheduled tasks are allowed to touch.
// episodicIngesterAdapter adapts memory.EpisodicIngester to the
// compute.EpisodicIngester interface. They share the same shape
// but can't import each other without a package cycle, so a thin
// adapter keeps the types at the right layer boundary.
type episodicIngesterAdapter struct {
	inner *memory.EpisodicIngester
}

func (a *episodicIngesterAdapter) IngestTurn(ctx context.Context, t compute.EpisodicTurn) error {
	return a.inner.IngestTurn(ctx, memory.EpisodicTurn{
		Channel:     t.Channel,
		ChatID:      t.ChatID,
		UserID:      t.UserID,
		Owner:       t.Owner,
		UserMessage: t.UserMessage,
		AssistReply: t.AssistReply,
		TurnID:      t.TurnID,
		CompletedAt: t.CompletedAt,
	})
}

// contextBudgetFromConfig maps [compute.context] onto the agent's
// budget. Each knob is a pointer in config so an omitted key takes
// the default while an explicit 0 genuinely disables that bound —
// operators who want the old unbounded replay have to ask for it.
func contextBudgetFromConfig(cfg config.ContextConfig) compute.ContextBudget {
	out := compute.DefaultContextBudget()
	if cfg.TailTokens != nil {
		out.TailTokens = *cfg.TailTokens
	}
	if cfg.HistoryToolResultBytes != nil {
		out.HistoryToolResultBytes = *cfg.HistoryToolResultBytes
	}
	return out
}

// serverToolsFromConfig converts the TOML-shaped ServerToolSpec
// list into the compute-layer ServerTool shape. Trivial mapper; the
// separation just keeps config types out of internal/compute.
func serverToolsFromConfig(in []config.ServerToolSpec) []compute.ServerTool {
	if len(in) == 0 {
		return nil
	}
	out := make([]compute.ServerTool, 0, len(in))
	for _, s := range in {
		out = append(out, compute.ServerTool{
			Type:       s.Type,
			Parameters: s.Parameters,
		})
	}
	return out
}

func (n *Node) schedulerClaims(creator string) *types.Claims {
	if creator == "" {
		creator = "scheduler"
	}
	return &types.Claims{
		UserID: creator,
		Scope:  "scheduler",
	}
}

// wireGateway builds the REST server + any channel handlers listed
// in cfg.Gateway.Channels. The channel list is the extension point:
// each entry is discriminated by Type and dispatched to a handler
// constructor. Unknown types log a warning and skip rather than
// aborting boot — a typo in a single channel shouldn't prevent the
// rest of the gateway from coming up.
//
// Today's supported types: "telegram". The REST surface (/v1/messages,
// /healthz, /readyz, /v1/prompts/...) is always mounted when the
// gateway function is enabled — it's the control plane, not a channel
// in the list. Adding a new chat backend (Slack, Matrix, Signal) is
// a new case plus a handler package; the config shape doesn't change.

// runDetect runs a [[binary]].detect command and returns its
// combined stdout+stderr. Used by the boot auto-install loop to
// extract the currently-installed version. Errors are silenced —
// the empty string just means "version-check inconclusive" and
// the caller proceeds via the regular Available path.
func runDetect(ctx context.Context, cmdline string) string {
	parts := strings.Fields(cmdline)
	if len(parts) == 0 {
		return ""
	}
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

func truncateLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}
