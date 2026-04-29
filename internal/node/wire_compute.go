package node

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cryptorand "crypto/rand"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/compute/research"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/modelsdev"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/pkg/config"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

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

	n.toolRegistry = compute.NewRegistry()
	n.executor = compute.NewExecutor(n.toolRegistry, n.policyEngine, n.hooksDisp, compute.ExecutorConfig{}, n.log)

	// Stdlib builtins: cheap Go-native tools every node ships with
	// (current_time today, more to follow). Register the handlers
	// into the Builtins registry and the ToolDefs into the exec
	// Registry so the LLM sees them in its function-calling list.
	// Failures here are config bugs, not runtime — bubble up.
	builtins := compute.NewBuiltins()
	if err := compute.RegisterStdlibBuiltins(builtins); err != nil {
		return fmt.Errorf("builtins: %w", err)
	}
	n.executor.SetBuiltins(builtins)
	n.builtinsRegistry = builtins
	for _, t := range compute.StdlibToolDefs() {
		if err := n.toolRegistry.Register(t); err != nil {
			return fmt.Errorf("register stdlib tool %q: %w", t.Name, err)
		}
	}

	// Embedding client (optional). When configured, memory_search
	// upgrades from substring to semantic vector search, and the
	// episodic ingester writes a paired vector record per turn.
	var embedder compute.EmbeddingProvider
	if n.cfg.Compute.Embeddings.Endpoint != "" {
		embKey, err := n.resolveAPIKey(n.cfg.Compute.Embeddings.APIKeyRef)
		if err != nil {
			return fmt.Errorf("embeddings api key: %w", err)
		}
		ec, err := compute.NewEmbeddingClient(compute.EmbeddingClientConfig{
			Endpoint: n.cfg.Compute.Embeddings.Endpoint,
			APIKey:   embKey,
			Model:    n.cfg.Compute.Embeddings.Model,
			Dims:     n.cfg.Compute.Embeddings.Dims,
			Format:   compute.EmbeddingFormat(n.cfg.Compute.Embeddings.Format),
			Logger:   n.log,
		})
		if err != nil {
			return fmt.Errorf("embedding client: %w", err)
		}
		embedder = ec
		n.embedder = ec
		n.log.Debug("compute: embedding client wired",
			"model", n.cfg.Compute.Embeddings.Model,
			"dims", n.cfg.Compute.Embeddings.Dims)
	}

	// Memory tools: registered when Raft + store are available.
	// Without these the model can't recall anything past the
	// conversation-history buffer. Safe to register unconditionally
	// on Raft-hosting nodes; a compute-only node (no Raft) skips.
	if n.raft != nil && n.store != nil {
		if err := compute.RegisterMemoryBuiltins(builtins, compute.MemoryConfig{
			Store:     n.store,
			Raft:      n.raft,
			Forgetter: n.memorySvc,
			Dreamer:   n.memorySvc,
			Embedder:  embedder,
		}); err != nil {
			return fmt.Errorf("register memory builtins: %w", err)
		}
		for _, td := range compute.MemoryToolDefs() {
			if err := n.toolRegistry.Register(td); err != nil {
				return fmt.Errorf("register memory tool %q: %w", td.Name, err)
			}
		}
		n.log.Debug("compute: memory_search + memory_write registered")
	}

	// Schedule tools: schedule_create / list / get / delete. Need
	// Raft + store. Agent-turn handler for the actual dispatch is
	// already registered via registerAgentTurnHandlers().
	if n.raft != nil && n.store != nil {
		if err := compute.RegisterScheduleBuiltins(builtins, compute.ScheduleConfig{
			Store: n.store,
			Raft:  n.raft,
		}); err != nil {
			return fmt.Errorf("register schedule builtins: %w", err)
		}
		for _, td := range compute.ScheduleToolDefs() {
			if err := n.toolRegistry.Register(td); err != nil {
				return fmt.Errorf("register schedule tool %q: %w", td.Name, err)
			}
		}
		n.log.Debug("compute: schedule_create/list/get/delete registered")

		// Commitments: one-shot due-at jobs (the right primitive for
		// "in 2 minutes message me"). Same Store + Raft pattern;
		// dispatches through the existing runCommitmentAsAgentTurn
		// handler.
		if err := compute.RegisterCommitmentBuiltins(builtins, compute.CommitmentConfig{
			Store: n.store,
			Raft:  n.raft,
		}); err != nil {
			return fmt.Errorf("register commitment builtins: %w", err)
		}
		for _, td := range compute.CommitmentToolDefs() {
			if err := n.toolRegistry.Register(td); err != nil {
				return fmt.Errorf("register commitment tool %q: %w", td.Name, err)
			}
		}
		n.log.Debug("compute: commitment_create/list/cancel registered")

		// Credentials + OAuth builtins. Tracker + Service are always
		// wired on raft-hosting nodes (wireCredentials); operator
		// declares IdPs via [security.oauth.<name>]. Empty Providers
		// is fine — oauth_start surfaces "not configured" at call
		// time. Default-deny policy seed gates these to scope:owner.
		if n.credentialSvc != nil && n.oauthTracker != nil {
			if err := compute.RegisterCredentialsBuiltins(builtins, compute.CredentialsConfig{
				Tracker:   n.oauthTracker,
				Service:   n.credentialSvc,
				Providers: n.oauthProviders,
			}); err != nil {
				return fmt.Errorf("register credentials builtins: %w", err)
			}
			for _, td := range compute.CredentialsToolDefs() {
				if err := n.toolRegistry.Register(td); err != nil {
					return fmt.Errorf("register credentials tool %q: %w", td.Name, err)
				}
			}
			n.log.Debug("compute: oauth_* + credentials_* registered",
				"providers", len(n.oauthProviders))
		}

		// Operator-declared host binary catalogue. Each [[binary]] in
		// config.toml becomes a BinaryDeclaration the agent can install
		// via binary_install(name). Same Satisfier + Manager pool as
		// clawhub_install — the only difference is the source of the
		// install spec (operator config vs. clawhub bundle).
		if len(n.cfg.Binaries) > 0 {
			satisfier := binaries.New(binaries.Config{
				HTTPClient:    egress.For("binaries-install").HTTPClient(),
				Logger:        n.log,
				InstallPrefix: n.cfg.Security.BinaryInstallPrefix,
			})
			decls := make(map[string]compute.BinaryDeclaration, len(n.cfg.Binaries))
			for _, b := range n.cfg.Binaries {
				install := make([]binaries.InstallSpec, 0, len(b.Install))
				for _, in := range b.Install {
					install = append(install, binaries.InstallSpec{
						OS: in.OS, Arch: in.Arch, Distro: in.Distro,
						Manager: in.Manager, Package: in.Package,
						Repo: in.Repo, URL: in.URL, Checksum: in.Checksum,
						Sudo: in.Sudo, Args: in.Args,
					})
				}
				decls[b.Name] = compute.BinaryDeclaration{
					Name:        b.Name,
					Description: b.Description,
					Detect:      b.Detect,
					Install:     install,
					PostInstall: b.PostInstall,
				}
			}
			if err := compute.RegisterBinariesBuiltins(builtins, compute.BinariesConfig{
				Satisfier:    satisfier,
				Declarations: decls,
			}); err != nil {
				return fmt.Errorf("register binaries builtins: %w", err)
			}
			for _, td := range compute.BinariesToolDefs() {
				if err := n.toolRegistry.Register(td); err != nil {
					return fmt.Errorf("register binaries tool %q: %w", td.Name, err)
				}
			}
			n.log.Debug("compute: binary_install + binary_list registered", "count", len(decls))

			// Async auto-install: for each declared binary, check
			// PATH; if missing, run satisfy in a goroutine. Failures
			// log a warning but don't block boot — operator can
			// re-run later via binary_install builtin (with
			// bootstrap_managers=true if a manager itself is
			// missing). Skip when the operator only declared
			// binaries that need bootstrap (we don't auto-opt-in
			// to running upstream installer scripts at boot).
			for name, decl := range decls {
				name, decl := name, decl
				go func() {
					if satisfier.Available(name) {
						n.log.Debug("binary: auto-install skip (already on PATH)", "name", name)
						return
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()
					n.log.Info("binary: auto-install starting", "name", name)
					res, err := satisfier.Satisfy(ctx, name, decl.Install)
					if err != nil {
						n.log.Warn("binary: auto-install failed (operator can re-run via binary_install)", "name", name, "err", err)
						return
					}
					n.log.Info("binary: auto-install done", "name", name, "manager", res.Manager, "already_available", res.AlreadyAvailable)
				}()
			}
		}

		// Clawhub install builtin: only registered when the operator
		// configured a clawhub base URL (i.e. wireClawhub built an
		// installer). Default-deny — owner-only via the noSeed list.
		if n.clawhubInstaller != nil {
			if err := compute.RegisterClawhubBuiltin(builtins, compute.ClawhubConfig{
				Installer:            n.clawhubInstaller,
				DefaultMount:         n.cfg.Security.ClawhubInstallMount,
				AutoEmitInstallRules: n.cfg.Security.ClawhubAutoEmitInstallRules,
				PolicyAdder:          n.policySvc,
				Logger:               n.log,
			}); err != nil {
				return fmt.Errorf("register clawhub builtin: %w", err)
			}
			for _, td := range compute.ClawhubToolDefs() {
				if err := n.toolRegistry.Register(td); err != nil {
					return fmt.Errorf("register clawhub tool %q: %w", td.Name, err)
				}
			}
			n.log.Debug("compute: clawhub_install registered")
		}

		// Deep-research builtin (research_start). Default-allow at
		// the policy seed layer like other builtins; operators add
		// an explicit deny rule when they want the agent's async
		// research runs gated.
		if err := compute.RegisterResearchBuiltins(builtins, compute.ResearchConfig{
			Raft: n.raft,
		}); err != nil {
			return fmt.Errorf("register research builtins: %w", err)
		}
		for _, td := range compute.ResearchToolDefs() {
			if err := n.toolRegistry.Register(td); err != nil {
				return fmt.Errorf("register research tool %q: %w", td.Name, err)
			}
		}
		n.log.Debug("compute: research_start registered")
	}

	// Council tools: list_providers + council_review. Only wire
	// when multiple providers are registered — a single-provider
	// deployment has nothing to council.
	if n.providerRegistry != nil && len(n.providerRegistry.List()) > 1 {
		if err := compute.RegisterCouncilBuiltins(builtins, compute.CouncilConfig{
			Registry: n.providerRegistry,
		}); err != nil {
			return fmt.Errorf("register council builtins: %w", err)
		}
		for _, td := range compute.CouncilToolDefs() {
			if err := n.toolRegistry.Register(td); err != nil {
				return fmt.Errorf("register council tool %q: %w", td.Name, err)
			}
		}
		n.log.Debug("compute: list_providers + council_review registered",
			"provider_count", len(n.providerRegistry.List()))
	}

	// Fetch tool is always-on — no secret required, the SSRF
	// guard blocks private addresses by default. Operators who
	// want to disable it write a deny rule against the fetch_url
	// tool name.
	if err := compute.RegisterFetchBuiltin(builtins, compute.FetchConfig{}); err != nil {
		return fmt.Errorf("register fetch_url: %w", err)
	}
	if err := n.toolRegistry.Register(compute.FetchToolDef()); err != nil {
		return fmt.Errorf("register fetch_url tool def: %w", err)
	}
	n.log.Debug("compute: fetch_url registered")

	// Write + edit tools — destructive so they register with
	// RiskIrreversible. Default policy seeding (below) adds an
	// allow rule but operators who want confirmation-on-every-write
	// override with a higher-priority require_confirmation rule.
	if err := compute.RegisterWriteEditBuiltins(builtins); err != nil {
		return fmt.Errorf("register write/edit: %w", err)
	}
	if err := n.toolRegistry.Register(compute.WriteToolDef()); err != nil {
		return fmt.Errorf("register write_file tool def: %w", err)
	}
	if err := n.toolRegistry.Register(compute.EditToolDef()); err != nil {
		return fmt.Errorf("register edit_file tool def: %w", err)
	}
	n.log.Debug("compute: write_file + edit_file registered")

	// Debug tools expose internal state (tools, policy rules,
	// memory stats, raft, scheduler, providers) so the agent can
	// answer operator questions like "what tools do you have"
	// or "what's the current raft leader" directly. Scope-level
	// gating is the operator's policy responsibility — a deny
	// rule against debug_* for non-owner scopes keeps strangers
	// out without needing a separate config toggle.
	if err := compute.RegisterDebugBuiltins(builtins, &debugInspector{n: n}); err != nil {
		return fmt.Errorf("register debug builtins: %w", err)
	}
	for _, td := range compute.DebugToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			return fmt.Errorf("register debug tool %q: %w", td.Name, err)
		}
	}
	n.log.Debug("compute: debug_* builtins registered")

	// Shell access — most dangerous of all the stdlib tools.
	// Denylist + compound-command gate + 30s default timeout
	// give an MVP-acceptable surface; the ask-based permission
	// model replaces this with per-pattern approval later.
	if err := compute.RegisterShellBuiltin(builtins); err != nil {
		return fmt.Errorf("register shell_command: %w", err)
	}
	if err := n.toolRegistry.Register(compute.ShellToolDef()); err != nil {
		return fmt.Errorf("register shell_command tool def: %w", err)
	}
	n.log.Debug("compute: shell_command registered")

	// Web search: only registered when an Exa API key is
	// configured. Skipped silently when absent so deployments that
	// don't want web access don't need to redact anything — they
	// just don't set the key.
	if n.cfg.Compute.WebSearch.APIKeyRef != "" {
		exaKey, err := n.resolveAPIKey(n.cfg.Compute.WebSearch.APIKeyRef)
		if err != nil {
			return fmt.Errorf("web_search api key: %w", err)
		}
		if exaKey != "" {
			if err := compute.RegisterWebSearchBuiltin(builtins, compute.WebSearchConfig{
				APIKey:   exaKey,
				Endpoint: n.cfg.Compute.WebSearch.Endpoint,
			}); err != nil {
				return fmt.Errorf("register web_search: %w", err)
			}
			if err := n.toolRegistry.Register(compute.WebSearchToolDef()); err != nil {
				return fmt.Errorf("register web_search tool def: %w", err)
			}
			n.log.Debug("compute: web_search (Exa) registered")
		}
	}

	// models.dev capability auto-discovery: when any provider has
	// auto_capabilities = true, fetch the catalog (24h disk cache),
	// look up each opt-in provider's model, MERGE discovered modalities
	// into the declared capabilities. Declared always wins on conflict.
	// No-op when no provider opts in. Failures are non-fatal — operator
	// keeps whatever they declared. Boot-time fetch — own 30s timeout
	// inside the fetcher, no parent ctx needed.
	n.applyModelsDevAutoCapabilities(context.Background())

	// Vision: read_image builtin. Resolution order:
	//   1. provider="<label>" → inherit endpoint/model/key/format
	//      from that [[compute.providers]] entry.
	//   2. inline endpoint+api_key_ref → use directly.
	//   3. neither → builtin not registered; agent honestly tells
	//      the user it can't view images.
	if visionEP := n.resolveVisionEndpoint(); visionEP != nil {
		if err := compute.RegisterVisionBuiltin(builtins, compute.VisionConfig{
			Endpoint: visionEP.endpoint,
			Model:    visionEP.model,
			APIKey:   visionEP.apiKey,
			Format:   compute.VisionFormat(visionEP.format),
		}); err != nil {
			return fmt.Errorf("register read_image: %w", err)
		}
		if err := n.toolRegistry.Register(compute.VisionToolDef()); err != nil {
			return fmt.Errorf("register read_image tool def: %w", err)
		}
		n.log.Debug("compute: read_image registered",
			"model", visionEP.model, "format", visionEP.format, "via", visionEP.via)
	}

	// Audio (STT): read_audio builtin, same provider-reference shape
	// as vision. Whisper-compatible multipart POST regardless of
	// whether the endpoint is OpenAI, MiniMax, or a self-hosted
	// faster-whisper / parakeet sidecar exposing the same surface.
	if audioEP := n.resolveAudioEndpoint(); audioEP != nil {
		audioFmt := compute.AudioFormatWhisper
		if audioEP.matchedCap == compute.CapabilityAudioMultimodal {
			audioFmt = compute.AudioFormatChatMultimodal
		}
		if err := compute.RegisterAudioBuiltin(builtins, compute.AudioConfig{
			Endpoint: audioEP.endpoint,
			Model:    audioEP.model,
			APIKey:   audioEP.apiKey,
			Format:   audioFmt,
		}); err != nil {
			return fmt.Errorf("register read_audio: %w", err)
		}
		if err := n.toolRegistry.Register(compute.AudioToolDef()); err != nil {
			return fmt.Errorf("register read_audio tool def: %w", err)
		}
		n.log.Debug("compute: read_audio registered",
			"model", audioEP.model, "format", audioFmt, "via", audioEP.via)
	}

	// Soul self-tuning builtins. Default-deny via the seedDefault
	// rules below — operators open per-scope so only the owner's
	// chat can mutate the agent's identity. soul_get is also
	// default-deny by virtue of being in the soul_* namespace;
	// tighten/loosen per scope as needed.
	if n.soulAdjuster != nil {
		if err := compute.RegisterSoulBuiltins(builtins, compute.SoulBuiltinsConfig{
			Mutator: n.soulAdjuster,
		}); err != nil {
			return fmt.Errorf("register soul builtins: %w", err)
		}
		for _, td := range compute.SoulToolDefs() {
			if err := n.toolRegistry.Register(td); err != nil {
				return fmt.Errorf("register soul tool def %q: %w", td.Name, err)
			}
		}
		n.log.Debug("compute: soul_* builtins registered")
	}

	// PDF: read_pdf builtin, capability="pdf". Same chat-completions
	// shape as audio-multimodal — content part {type:"file"} with
	// base64 PDF data. OpenRouter is the easy on-ramp; Anthropic
	// native PDF and Gemini PDF can land as additional formats.
	if pdfEP := n.resolvePDFEndpoint(); pdfEP != nil {
		if err := compute.RegisterPDFBuiltin(builtins, compute.PDFConfig{
			Endpoint: pdfEP.endpoint,
			Model:    pdfEP.model,
			APIKey:   pdfEP.apiKey,
		}); err != nil {
			return fmt.Errorf("register read_pdf: %w", err)
		}
		if err := n.toolRegistry.Register(compute.PDFToolDef()); err != nil {
			return fmt.Errorf("register read_pdf tool def: %w", err)
		}
		n.log.Debug("compute: read_pdf registered",
			"model", pdfEP.model, "via", pdfEP.via)
	}

	// Wire the skill registry's PolicySink so skill-bundled policy.d/
	// subtrees apply to the tool registry during scan. Order matters:
	// skills scanned BEFORE operator's policy.d load means
	// operator-authored policies win on overlap (SANDBOX.md §
	// "Skill-bundled policies" step 2).
	if n.skillRegistry != nil {
		n.skillRegistry.SetPolicySink(n.toolRegistry)
	}

	// Resolver from providers/chains. Nil if no providers are
	// configured — Agent stays constructible but LLM calls fail
	// until operator wires providers.
	if len(n.cfg.Compute.Providers) > 0 {
		r, err := compute.NewResolver(&n.cfg.Compute)
		if err != nil {
			return fmt.Errorf("resolver: %w", err)
		}
		n.resolver = r
	}

	// LLM provider build: injection wins for the main slot; else
	// build a client per configured [[compute.providers]] entry
	// and resolve the RoleMap against their labels.
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
				return fmt.Errorf("provider %q: %w", p.Label, err)
			}
			apiKey, err := n.resolveAPIKey(p.APIKeyRef)
			if err != nil {
				return fmt.Errorf("api key for provider %q: %w", p.Label, err)
			}
			client, err := compute.NewLLMClient(compute.LLMClientConfig{
				Endpoint:    p.Endpoint,
				APIKey:      apiKey,
				Model:       p.Model,
				ServerTools: serverToolsFromConfig(p.ServerTools),
				Logger:      n.log,
			})
			if err != nil {
				return fmt.Errorf("llm client for %q: %w", p.Label, err)
			}
			clientsByLabel[p.Label] = client
			n.providerRegistry.Register(compute.ProviderEntry{
				Label:        p.Label,
				TrustTier:    p.TrustTier,
				Capabilities: p.Capabilities,
				Backup:       p.Backup,
				Client:       client,
			})
			if i == 0 {
				n.llmProvider = client
			}
		}
	}

	// Explicit role map from config overrides fallback picks.
	n.roleMap = nil
	if n.llmProvider != nil {
		roleAssignments := map[compute.Role]compute.LLMProvider{}
		pickRole := func(role compute.Role, label string) error {
			if label == "" {
				return nil
			}
			c, ok := clientsByLabel[label]
			if !ok {
				return fmt.Errorf("compute.roles.%s: unknown provider label %q", role, label)
			}
			roleAssignments[role] = c
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
		if override, ok := roleAssignments[compute.RoleMain]; ok {
			main = override
			n.llmProvider = override
		}
		rm, err := compute.NewRoleMap(main, roleAssignments)
		if err != nil {
			return fmt.Errorf("role map: %w", err)
		}
		n.roleMap = rm
	}

	// Agent is only constructable with a non-nil Provider. A
	// Compute-enabled node with no providers gets n.agent=nil —
	// REST handler surfaces "provider not configured" at message
	// time rather than blocking boot.
	if n.llmProvider != nil {
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
		primaryLabel := ""
		if len(n.cfg.Compute.Providers) > 0 {
			primaryLabel = n.cfg.Compute.Providers[0].Label
			if n.cfg.Compute.Roles.Main != "" {
				primaryLabel = n.cfg.Compute.Roles.Main
			}
		}
		a, err := compute.NewAgent(compute.AgentConfig{
			Provider:     n.llmProvider,
			PrimaryLabel: primaryLabel,
			Providers:    n.providerRegistry,
			Executor:     n.executor,
			Registry:     n.toolRegistry,
			Soul: func() *types.SoulConfig {
				s := n.Soul()
				if s == nil {
					return nil
				}
				return &s.Config
			},
			EpisodicIngester: episodicIngester,
			Roles:            n.roleMap,
			ContextEngine: compute.NewContextEngine(compute.ContextEngineConfig{
				Store:    n.store,
				Embedder: n.embedder,
				Logger:   n.log,
			}),
			Skills:           skillDispatcherOrNil(n.skillAdapter),
			TimezoneResolver: n.resolveUserTimezone,
			Logger:           n.log,
		})
		if err != nil {
			return fmt.Errorf("agent: %w", err)
		}
		n.agent = a
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
	}

	n.log.Info("compute stack wired",
		"has_policy_engine", n.policyEngine != nil,
		"providers", len(n.cfg.Compute.Providers),
		"chains", len(n.cfg.Compute.Chains),
		"has_agent", n.agent != nil,
	)
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
	endpoint   string
	model      string
	apiKey     string
	format     string
	via        string // "override:<label>", "capability:<label>"
	matchedCap string // empty if override; else the capability that matched
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

// resolveModalityEndpoint discovers a provider for the given
// modality. Resolution order:
//
//  1. If override.Provider is set, pin to that label.
//  2. Otherwise scan [[compute.providers]] for any matching
//     capability (anyOf), highest Priority first.
//  3. Skip cleanly (return nil) when nothing matches — the caller
//     omits the builtin and the agent honestly reports it can't.
func (n *Node) resolveModalityEndpoint(modality, overrideLabel string, anyOf ...string) *llmEndpoint {
	if overrideLabel != "" {
		p := n.findProvider(overrideLabel)
		if p == nil {
			n.log.Warn("compute: "+modality+" override references unknown provider; skipping",
				"label", overrideLabel)
			return nil
		}
		return n.endpointFromProvider(modality, *p, "override:"+overrideLabel)
	}
	for _, want := range anyOf {
		matches := compute.SelectByCapability(n.cfg.Compute.Providers, want)
		for _, p := range matches {
			ep := n.endpointFromProvider(modality, p, "capability:"+p.Label)
			if ep != nil {
				ep.matchedCap = want
				return ep
			}
		}
	}
	return nil
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
	}
}

func (n *Node) resolveVisionEndpoint() *llmEndpoint {
	return n.resolveModalityEndpoint("vision", n.cfg.Compute.Vision.Provider, compute.CapabilityVision)
}

func (n *Node) resolveAudioEndpoint() *llmEndpoint {
	return n.resolveModalityEndpoint("audio", n.cfg.Compute.Audio.Provider,
		compute.CapabilityAudioTranscribe, compute.CapabilityAudioMultimodal)
}

func (n *Node) resolvePDFEndpoint() *llmEndpoint {
	return n.resolveModalityEndpoint("pdf", n.cfg.Compute.PDF.Provider, compute.CapabilityPDF)
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
		if p.AutoCapabilities {
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
		if !p.AutoCapabilities {
			continue
		}
		// Provider hint = endpoint hostname (lookupInProvider does
		// substring match against catalog providers' API URLs).
		hint := p.Endpoint
		matches := cat.LookupAll(p.Model)
		if len(matches) == 0 {
			if hinted, ok := cat.Lookup(hint, p.Model); ok {
				matches = []modelsdev.Model{hinted}
			}
		}
		if len(matches) == 0 {
			n.log.Info("modelsdev: model not found in catalog; using declared caps only",
				"label", p.Label, "model", p.Model)
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
}

// researchIDEntropy is a process-wide ULID monotonic source for
// research record IDs. Each adapter call hits this.
var researchIDEntropy = ulid.Monotonic(cryptorand.Reader, 0)

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
	id := "research-" + ulid.MustNew(ulid.Now(), researchIDEntropy).String()
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
		UserMessage: t.UserMessage,
		AssistReply: t.AssistReply,
		TurnID:      t.TurnID,
		CompletedAt: t.CompletedAt,
	})
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
