package node

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/compute/drivers/gemini"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/pkg/promptgen"
)

// registerAgentTools registers every builtin handler and its matching
// ToolDef into the shared registries, then returns the binaries prompt
// provider — the one piece of state a later stage (Agent construction)
// needs back out of this pass.
//
// The call order here is the wiring order and is load-bearing. Two
// dependencies in particular: the models.dev capability merge must run
// before the modality blocks, because those select a provider by
// capability and would otherwise only see what the operator declared
// by hand; and the whole pass must precede Agent construction, which
// snapshots the tool registry.
func (n *Node) registerAgentTools(builtins *compute.Builtins, embedder compute.EmbeddingProvider) (func() []promptgen.BinaryInfo, error) {
	var binariesProvider func() []promptgen.BinaryInfo

	// Everything in this block persists through Raft and reads back
	// through the store. Without them the model can't recall anything
	// past the conversation-history buffer and has nowhere to durably
	// park a schedule, so a compute-only node (no Raft) skips the lot.
	if n.raft != nil && n.store != nil {
		if err := n.wireMemoryTools(builtins, embedder); err != nil {
			return nil, err
		}
		if err := n.wireScheduleTools(builtins); err != nil {
			return nil, err
		}
		if err := n.wireCommitmentTools(builtins); err != nil {
			return nil, err
		}
		if err := n.wireCredentialsTools(builtins); err != nil {
			return nil, err
		}
		provider, err := n.wireBinariesTools(builtins)
		if err != nil {
			return nil, err
		}
		binariesProvider = provider
		if err := n.wireClawhubTools(builtins); err != nil {
			return nil, err
		}
		if err := n.wireResearchTools(builtins); err != nil {
			return nil, err
		}
	}

	if err := n.wireFetchTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireWriteEditTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireDebugTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireShellTools(builtins); err != nil {
		return nil, err
	}
	// After shell, deliberately: the two are the same kind of tool
	// pointed at different machines, and reading them together is how
	// the next person sees that shell_command is the LOCAL one.
	if err := n.wireRemoteTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireWebSearchTools(builtins); err != nil {
		return nil, err
	}

	// models.dev capability auto-discovery: when any provider has
	// auto_capabilities = true, fetch the catalog (24h disk cache),
	// look up each opt-in provider's model, MERGE discovered modalities
	// into the declared capabilities. Declared always wins on conflict.
	// No-op when no provider opts in. Failures are non-fatal — operator
	// keeps whatever they declared. Boot-time fetch — own 30s timeout
	// inside the fetcher, no parent ctx needed.
	//
	// Sits ahead of the modality blocks below on purpose: they resolve
	// their endpoint by capability, so they have to see the merged set.
	n.applyModelsDevAutoCapabilities(context.Background())

	if err := n.wireSkillViewTool(builtins); err != nil {
		return nil, err
	}
	if err := n.wireVisionTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireAudioTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireSoulTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wirePDFTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireSpeakTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireImageTools(builtins); err != nil {
		return nil, err
	}
	if err := n.wireVideoTools(builtins); err != nil {
		return nil, err
	}

	return binariesProvider, nil
}

// wireStdlibTools builds the Builtins registry every other subsystem
// registers into and seeds it with the cheap Go-native tools every
// node ships with (current_time today, more to follow). Handlers land
// in the Builtins registry, ToolDefs in the exec Registry so the LLM
// sees them in its function-calling list. Failures here are config
// bugs, not runtime — bubble up.
func (n *Node) wireStdlibTools() (*compute.Builtins, error) {
	builtins := compute.NewBuiltins()
	// Before any modality registers: failoverBuiltin reads the tracker
	// off the registry at Register time, so wiring it after would give
	// every chain a nil one.
	builtins.SetHealth(n.providerHealth)
	// Same reason as the health tracker: failoverBuiltin reads this off
	// the registry at Register time, so wiring it after the modalities
	// register would give every chain a nil floor — which permits
	// everything, and would be the failure mode hardest to notice.
	builtins.SetTrustFloor(n.trustFloorAccessor())
	if err := compute.RegisterStdlibBuiltins(builtins); err != nil {
		return nil, fmt.Errorf("builtins: %w", err)
	}
	n.executor.SetBuiltins(builtins)
	n.builtinsRegistry = builtins
	for _, t := range compute.StdlibToolDefs() {
		if err := n.toolRegistry.Register(t); err != nil {
			return nil, fmt.Errorf("register stdlib tool %q: %w", t.Name, err)
		}
	}

	// Pinned memory only exists where raft does. A node without it
	// omits the tools rather than registering ones that fail — an
	// advertised tool that always errors teaches the model it has a
	// capability it does not.
	if n.pinnedStore != nil {
		if err := compute.RegisterPinnedBuiltins(builtins, pinnedStoreAdapter{inner: n.pinnedStore}); err != nil {
			return nil, fmt.Errorf("pinned builtins: %w", err)
		}
		for _, t := range compute.PinnedToolDefs() {
			if err := n.toolRegistry.Register(t); err != nil {
				return nil, fmt.Errorf("register pinned tool %q: %w", t.Name, err)
			}
		}
	}
	return builtins, nil
}

// wireEmbedder builds the embedding client (optional). When
// configured, memory_search upgrades from substring to semantic vector
// search, and the episodic ingester writes a paired vector record per
// turn.
func (n *Node) wireEmbedder() (compute.EmbeddingProvider, error) {
	// Switched on explicitly rather than defaulted, so a typo is a
	// start-up error instead of a silent fall-through to the remote
	// path — which for type = "biultin" would then complain about a
	// missing endpoint and send the operator looking in the wrong
	// place entirely.
	switch t := n.cfg.Compute.Embeddings.Type; t {
	case "", "remote":
	case "builtin":
		return n.wireBuiltinEmbedder()
	default:
		return nil, fmt.Errorf("[compute.embeddings] type = %q is not a known type (want \"remote\" or \"builtin\")", t)
	}
	if n.cfg.Compute.Embeddings.Endpoint == "" {
		// Said out loud, because the consequence is invisible
		// otherwise: recall still works, but it matches words rather
		// than meaning, so "what do I use for config?" will not find
		// "prefers TOML". That is a reasonable way to run a node —
		// it is the default — but it should not be something an
		// operator has to infer from silence.
		n.log.Info("compute: no [embeddings] endpoint configured — " +
			"memory recall is lexical, not semantic")
		return nil, nil
	}
	embKey, err := n.resolveAPIKey(n.cfg.Compute.Embeddings.APIKeyRef)
	if err != nil {
		return nil, fmt.Errorf("embeddings api key: %w", err)
	}
	// Resolved at BOOT: an embeddings block naming a driver this node
	// cannot build should fail on start-up, not on the first recall.
	embFactory, err := n.drivers().EmbeddingFactory(n.cfg.Compute.Embeddings.Format)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	ec, err := compute.NewEmbeddingClient(compute.EmbeddingClientConfig{
		Endpoint:      n.cfg.Compute.Embeddings.Endpoint,
		DriverFactory: embFactory,
		APIKey:        embKey,
		Model:         n.cfg.Compute.Embeddings.Model,
		Dims:          n.cfg.Compute.Embeddings.Dims,
		Logger:        n.log,
	})
	if err != nil {
		return nil, fmt.Errorf("embedding client: %w", err)
	}
	n.log.Debug("compute: embedding client wired",
		"model", n.cfg.Compute.Embeddings.Model,
		"dims", n.cfg.Compute.Embeddings.Dims)
	return ec.WithPrefixes(n.cfg.Compute.Embeddings.QueryPrefix,
		n.cfg.Compute.Embeddings.PassagePrefix), nil
}

// wireMemoryTools registers memory_search / memory_write and friends,
// plus the conversation-transcript tools. The latter are distinct from
// memory_search, which answers "what do I know about X" semantically;
// these find literal text in a specific thread.
func (n *Node) wireMemoryTools(builtins *compute.Builtins, embedder compute.EmbeddingProvider) error {
	if err := compute.RegisterMemoryBuiltins(builtins, compute.MemoryConfig{
		Store:      n.store,
		Raft:       n.raft,
		Forgetter:  n.memorySvc,
		Dreamer:    n.memorySvc,
		Embedder:   embedder,
		CrossOwner: n.crossOwnerAuthz(),
	}); err != nil {
		return fmt.Errorf("register memory builtins: %w", err)
	}
	for _, td := range compute.MemoryToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			return fmt.Errorf("register memory tool %q: %w", td.Name, err)
		}
	}
	n.log.Debug("compute: memory_search + memory_write registered")

	return n.registerSessionTools()
}

// wireScheduleTools registers schedule_create / list / get / delete.
// The agent-turn handler for the actual dispatch is registered
// separately via registerAgentTurnHandlers().
func (n *Node) wireScheduleTools(builtins *compute.Builtins) error {
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
	return nil
}

// wireCommitmentTools registers the one-shot due-at jobs that are the
// right primitive for "in 2 minutes message me". Same Store + Raft
// pattern as schedules; dispatches through the existing
// runCommitmentAsAgentTurn handler.
func (n *Node) wireCommitmentTools(builtins *compute.Builtins) error {
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
	return nil
}

// wireCredentialsTools registers the credentials + OAuth builtins.
// Tracker + Service are always wired on raft-hosting nodes
// (wireCredentials); operator declares IdPs via
// [security.oauth.<name>]. Empty Providers is fine — oauth_start
// surfaces "not configured" at call time. Default-deny policy seed
// gates these to scope:owner.
func (n *Node) wireCredentialsTools(builtins *compute.Builtins) error {
	if n.credentialSvc == nil || n.oauthTracker == nil {
		return nil
	}
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
	return nil
}

// wireBinariesTools registers the operator-declared host binary
// catalogue. Each [[binary]] in config.toml becomes a
// BinaryDeclaration the agent can install via binary_install(name).
// Same Satisfier + Manager pool as clawhub_install — the only
// difference is the source of the install spec (operator config vs.
// clawhub bundle).
//
// Returns the promptgen provider the Agent uses to advertise the
// catalogue in its system prompt; nil when no [[binary]] is declared,
// because only this block has the declarations and satisfier needed to
// build one.
func (n *Node) wireBinariesTools(builtins *compute.Builtins) (func() []promptgen.BinaryInfo, error) {
	if len(n.cfg.Binaries) == 0 {
		return nil, nil
	}
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
			Version:     b.Version,
			HelpCommand: b.HelpCommand,
			Install:     install,
			PostInstall: b.PostInstall,
			Env:         b.Env,
		}
	}
	if err := compute.RegisterBinariesBuiltins(builtins, compute.BinariesConfig{
		Satisfier:     satisfier,
		Declarations:  decls,
		InstallPrefix: n.cfg.Security.BinaryInstallPrefix,
	}); err != nil {
		return nil, fmt.Errorf("register binaries builtins: %w", err)
	}
	for _, td := range compute.BinariesToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			return nil, fmt.Errorf("register binaries tool %q: %w", td.Name, err)
		}
	}
	n.log.Debug("compute: binary_install + binary_list registered", "count", len(decls))

	installPrefix := n.cfg.Security.BinaryInstallPrefix
	binariesProvider := func() []promptgen.BinaryInfo {
		out := make([]promptgen.BinaryInfo, 0, len(decls))
		for _, d := range decls {
			out = append(out, promptgen.BinaryInfo{
				Name:        d.Name,
				Description: d.Description,
				PostInstall: d.PostInstall,
				Help:        binaries.ReadHelp(installPrefix, d.Name),
				Installed:   satisfier.Available(d.Name),
			})
		}
		return out
	}

	// Async auto-install: for each declared binary, check PATH; if
	// missing, run Satisfy in a goroutine. See autoInstallBinary for
	// the non-blocking guarantees that keep boot moving.
	for name, decl := range decls {
		go n.autoInstallBinary(satisfier, name, decl)
	}

	return binariesProvider, nil
}

// autoInstallBinary brings one declared [[binary]] up to the declared
// version, in the background so boot is never blocked.
//
// Non-blocking guarantees:
//   - Runs in its own goroutine — wireCompute returns immediately,
//     boot continues.
//   - Per-call 5-minute context timeout caps any single hang (e.g.
//     unreachable upstream).
//   - Panic recovery ensures a Satisfier or Manager bug can't take
//     down the node.
//   - Every code path ends in a log line — never silent.
//
// Bootstrap is intentionally off here: auto-install at boot does NOT
// run upstream curl-sh installers (brew, uv, bun) without explicit
// operator consent. Bundles requiring those need the operator to call
// binary_install with bootstrap_managers=true.
func (n *Node) autoInstallBinary(satisfier *binaries.Satisfier, name string, decl compute.BinaryDeclaration) {
	defer func() {
		if r := recover(); r != nil {
			n.log.Error("binary: auto-install panicked",
				"name", name, "panic", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Resolve version="latest" against GitHub's releases API, then
	// substitute {{version}} and {{stripv}} into the install specs'
	// URLs. Skip resolution if version isn't "latest" or no gh-release
	// spec is declared.
	declVersion := decl.Version
	declInstall := decl.Install
	if strings.EqualFold(declVersion, "latest") {
		if resolver := satisfier.LookupLatestResolver(); resolver != nil {
			for _, spec := range declInstall {
				if spec.Manager == "gh-release" && spec.URL != "" {
					latest, err := resolver.ResolveLatest(ctx, spec.URL)
					if err != nil {
						n.log.Warn("binary: latest-resolve failed; falling back to PATH check",
							"name", name, "err", err)
						declVersion = ""
						break
					}
					declVersion = strings.TrimPrefix(latest, "v")
					declInstall = binaries.SubstituteVersion(declInstall, latest)
					n.log.Info("binary: resolved latest",
						"name", name, "version", declVersion)
					break
				}
			}
		}
	} else if declVersion != "" {
		declVersion = strings.TrimPrefix(declVersion, "v")
		declInstall = binaries.SubstituteVersion(declInstall, declVersion)
	}

	// captureHelp persists --help output for the agent.
	// fresh=true → always recapture (post-install); fresh=false →
	// only capture when help.txt is missing (self-heal for
	// binaries already on PATH from a prior run).
	captureHelp := func(fresh bool) {
		if !fresh && binaries.ReadHelp(n.cfg.Security.BinaryInstallPrefix, decl.Name) != "" {
			return
		}
		helpOut := binaries.CaptureHelp(ctx, decl.Name, decl.HelpCommand)
		if helpOut == "" {
			return
		}
		if err := binaries.WriteHelp(n.cfg.Security.BinaryInstallPrefix, decl.Name, helpOut); err != nil {
			n.log.Warn("binary: help capture write failed",
				"name", name, "err", err)
		} else {
			n.log.Debug("binary: help captured",
				"name", name, "bytes", len(helpOut), "fresh", fresh)
		}
	}

	// applyEnvWrapper is idempotent — safe on every code path.
	// Self-heals when the operator added an env block to a binary
	// that was already installed.
	applyEnvWrapper := func() {
		if len(decl.Env) == 0 {
			return
		}
		status, werr := binaries.EnsureEnvWrapper(n.cfg.Security.BinaryInstallPrefix, decl.Name, decl.Env)
		if werr != nil {
			n.log.Warn("binary: env wrapper failed",
				"name", name, "err", werr)
			return
		}
		if status != binaries.WrapperUnchanged {
			n.log.Info("binary: env wrapper",
				"name", name, "status", status.String())
		}
	}

	// Version-mismatch upgrade path: if the operator declared a
	// version + detect command, run detect and check whether its
	// stdout contains the declared (or resolved) version. Mismatch →
	// force reinstall. Available alone (no version) → skip if on PATH.
	force := false
	reason := ""
	if declVersion != "" && decl.Detect != "" && satisfier.Available(name) {
		currentOut := runDetect(ctx, decl.Detect)
		if !strings.Contains(currentOut, declVersion) {
			force = true
			reason = "version mismatch (declared=" + declVersion + ", detect=" + truncateLine(currentOut, 80) + ")"
		} else {
			n.log.Debug("binary: auto-install skip (version match)",
				"name", name, "version", declVersion)
			captureHelp(false)
			applyEnvWrapper()
			return
		}
	} else if satisfier.Available(name) {
		n.log.Debug("binary: auto-install skip (already on PATH)", "name", name)
		captureHelp(false)
		applyEnvWrapper()
		return
	}
	if force {
		n.log.Info("binary: auto-upgrade starting", "name", name, "reason", reason)
	} else {
		n.log.Info("binary: auto-install starting", "name", name)
	}
	res, err := satisfier.SatisfyOpts(ctx, name, declInstall, binaries.SatisfyOptions{Force: force})
	if err != nil {
		n.log.Warn("binary: auto-install failed (operator can re-run via binary_install)",
			"name", name, "err", err)
		return
	}
	n.log.Info("binary: auto-install done",
		"name", name,
		"manager", res.Manager,
		"already_available", res.AlreadyAvailable,
		"forced_upgrade", force)

	applyEnvWrapper()
	captureHelp(true)
}

// wireClawhubTools registers the clawhub install builtin. Only
// registered when the operator configured a clawhub base URL (i.e.
// wireClawhub built an installer). Default-deny — owner-only via the
// noSeed list.
func (n *Node) wireClawhubTools(builtins *compute.Builtins) error {
	if n.clawhubInstaller == nil {
		return nil
	}
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
	return nil
}

// wireResearchTools registers the deep-research builtin
// (research_start). Default-allow at the policy seed layer like other
// builtins; operators add an explicit deny rule when they want the
// agent's async research runs gated.
func (n *Node) wireResearchTools(builtins *compute.Builtins) error {
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
	return nil
}

// wireCouncilTools registers list_providers + council_review. Only
// wired when multiple providers are registered — a single-provider
// deployment has nothing to council.
func (n *Node) wireCouncilTools(builtins *compute.Builtins) error {
	if n.providerRegistry == nil || len(n.providerRegistry.List()) <= 1 {
		return nil
	}
	if err := compute.RegisterCouncilBuiltins(builtins, compute.CouncilConfig{
		Registry: n.providerRegistry,
		Roles:    n.roleMap,
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
	return nil
}

// wireFetchTools registers fetch_url, which is always-on — no secret
// required, the SSRF guard blocks private addresses by default.
// Operators who want to disable it write a deny rule against the
// fetch_url tool name.
func (n *Node) wireFetchTools(builtins *compute.Builtins) error {
	if err := compute.RegisterFetchBuiltin(builtins, compute.FetchConfig{}); err != nil {
		return fmt.Errorf("register fetch_url: %w", err)
	}
	if err := n.toolRegistry.Register(compute.FetchToolDef()); err != nil {
		return fmt.Errorf("register fetch_url tool def: %w", err)
	}
	n.log.Debug("compute: fetch_url registered")
	return nil
}

// wireWriteEditTools registers write_file + edit_file — destructive so
// they register with RiskIrreversible. Default policy seeding adds an
// allow rule but operators who want confirmation-on-every-write
// override with a higher-priority require_confirmation rule.
func (n *Node) wireWriteEditTools(builtins *compute.Builtins) error {
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
	return nil
}

// wireDebugTools exposes internal state (tools, policy rules, memory
// stats, raft, scheduler, providers) so the agent can answer operator
// questions like "what tools do you have" or "what's the current raft
// leader" directly. Scope-level gating is the operator's policy
// responsibility — a deny rule against debug_* for non-owner scopes
// keeps strangers out without needing a separate config toggle.
func (n *Node) wireDebugTools(builtins *compute.Builtins) error {
	if err := compute.RegisterDebugBuiltins(builtins, &debugInspector{n: n}); err != nil {
		return fmt.Errorf("register debug builtins: %w", err)
	}
	for _, td := range compute.DebugToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			return fmt.Errorf("register debug tool %q: %w", td.Name, err)
		}
	}
	n.log.Debug("compute: debug_* builtins registered")
	return nil
}

// wireShellTools registers shell_command — most dangerous of all the
// stdlib tools. Denylist + compound-command gate + 30s default timeout
// give an MVP-acceptable surface; the ask-based permission model
// replaces this with per-pattern approval later.
func (n *Node) wireShellTools(builtins *compute.Builtins) error {
	if err := compute.RegisterShellBuiltin(builtins); err != nil {
		return fmt.Errorf("register shell_command: %w", err)
	}
	if err := n.toolRegistry.Register(compute.ShellToolDef()); err != nil {
		return fmt.Errorf("register shell_command tool def: %w", err)
	}
	n.log.Debug("compute: shell_command registered")
	return nil
}

// wireRemoteTools registers remote_ssh over the configured [[devbox]]
// blocks.
//
// Skipped silently when there are none: a deployment with no isolated
// host to dispatch to should not advertise a tool that refuses every
// call. But a block that is PRESENT and broken fails boot — an
// unreadable key or a bad host is a configuration error the operator
// needs at start-up, not a tool error surfacing mid-turn hours later
// and reading like a model fault.
func (n *Node) wireRemoteTools(builtins *compute.Builtins) error {
	if len(n.cfg.Remotes) == 0 {
		return nil
	}
	// Asked before the key is read and the set is built: resolving an
	// SSH key for a tool that will not be registered is work whose
	// only possible outcome is a boot failure over a tool the operator
	// switched off.
	if n.toolRegistry.Disabled("remote_ssh") {
		n.log.Info("compute: remote_ssh is disabled by compute.disabled_tools; " +
			"the [[remote]] blocks are not wired")
		return nil
	}
	set, err := compute.NewRemoteSet(n.cfg.Remotes, secretResolverFunc(n.resolveAPIKey))
	if err != nil {
		return err
	}
	if err := compute.RegisterRemoteBuiltins(builtins, set); err != nil {
		return fmt.Errorf("register remote_ssh: %w", err)
	}
	for _, td := range compute.RemoteToolDefs(set) {
		if err := n.toolRegistry.Register(td); err != nil {
			return fmt.Errorf("register %s tool def: %w", td.Name, err)
		}
	}
	n.log.Info("compute: remote_ssh registered", "devboxes", set.Names())
	return nil
}

// secretResolverFunc adapts the node's ref resolver to the narrow
// interface compute defines, so compute keeps not importing secrets.
type secretResolverFunc func(string) (string, error)

func (f secretResolverFunc) Resolve(ref string) (string, error) { return f(ref) }

// wireWebSearchTools registers web_search over the search backends
// config selects, in failover order. Skipped silently when none
// resolve, so a deployment that doesn't want web access doesn't need
// to redact anything — it just declares no search provider.
//
// Drivers are built at BOOT, like vision's. An unknown driver name or
// a template mapping missing its required fields is a configuration
// error the operator should see on start-up, not the first time
// somebody asks what the news is.
func (n *Node) wireWebSearchTools(builtins *compute.Builtins) error {
	providers := resolvedSearchProviders(n.cfg.Compute)
	if len(providers) == 0 {
		return nil
	}
	cfgs := make([]compute.WebSearchConfig, 0, len(providers))
	for _, p := range providers {
		apiKey, err := n.resolveAPIKey(p.APIKeyRef)
		if err != nil {
			return fmt.Errorf("web_search provider %q api key: %w", p.Label, err)
		}
		// A declared key that resolves to nothing is a redaction, not
		// a configuration to boot with: the operator asked for this
		// backend and the secret is missing.
		if p.APIKeyRef != "" && apiKey == "" {
			return fmt.Errorf("web_search provider %q: api_key_ref %q resolved empty", p.Label, p.APIKeyRef)
		}
		driver, err := n.drivers().Search(p.Driver, compute.SearchDriverConfig{
			Endpoint:    p.Endpoint,
			Credential:  searchCredential(p.Driver, apiKey),
			HTTPClient:  searchEgressClient(p.Timeout),
			Logger:      n.log,
			Options:     p.Options,
			ExtraParams: p.ExtraParams,
			Response:    p.Response,
		})
		if err != nil {
			return fmt.Errorf("web_search: provider %q: %w", p.Label, err)
		}
		cfgs = append(cfgs, compute.WebSearchConfig{
			Driver:    driver,
			Label:     p.Label,
			TrustTier: p.TrustTier,
		})
	}
	if err := compute.RegisterWebSearchBuiltin(builtins, cfgs...); err != nil {
		return fmt.Errorf("register web_search: %w", err)
	}
	labels := make([]string, 0, len(providers))
	for _, p := range providers {
		labels = append(labels, p.Label)
	}
	if err := n.toolRegistry.Register(compute.WebSearchToolDef(labels...)); err != nil {
		return fmt.Errorf("register web_search tool def: %w", err)
	}
	n.log.Debug("compute: web_search registered",
		"provider", providers[0].Label, "driver", providers[0].Driver,
		"chain_len", len(providers))
	return nil
}

// searchCredential builds the credential shape a search driver
// expects. Exa wants a bare x-api-key; everything else is handed a
// bearer credential and re-wraps it if its API disagrees — which is
// what the template driver's auth_style is for.
//
// Nil for no key at all, because that is the normal case for a private
// SearXNG and the reason web_search no longer gates registration on
// having a secret.
func searchCredential(driver, apiKey string) compute.Credential {
	if apiKey == "" {
		return nil
	}
	// Empty means Exa, the same default DriverSet.Search applies, so the
	// credential shape and the driver lookup cannot disagree.
	if d := normaliseSearchDriver(driver); d == "" || d == compute.DriverExa {
		return compute.ExaCredential(apiKey)
	}
	return compute.NewBearerCredential(apiKey)
}

// searchEgressClient returns the egress-routed client for web_search.
//
// Copied rather than used directly: egress.Client hands back one
// shared instance, and setting a per-provider timeout on it would
// change the deadline for every other caller of the same role. Same
// move builtin_fetch.go makes to wrap its SSRF guard.
//
// The timeout is set unconditionally because the search default is
// tighter than the egress default, and passing a non-nil client means
// the driver's own default never gets a say.
func searchEgressClient(timeout time.Duration) *http.Client {
	base := egress.For("web_search").HTTPClient()
	client := *base
	if timeout <= 0 {
		timeout = compute.DefaultSearchTimeout
	}
	client.Timeout = timeout
	return &client
}

// wireVisionTools registers the read_image builtin. Resolution order:
//
//  1. provider="<label>" → inherit endpoint/model/key/format from that
//     [[compute.providers]] entry.
//  2. inline endpoint+api_key_ref → use directly.
//  3. neither → builtin not registered; agent honestly tells the user
//     it can't view images.
func (n *Node) wireVisionTools(builtins *compute.Builtins) error {
	eps := n.resolveVisionEndpoints()
	if len(eps) == 0 {
		return nil
	}
	cfgs := make([]compute.VisionConfig, 0, len(eps))
	for _, ep := range eps {
		// Resolved at BOOT, not per call. An endpoint naming a driver
		// this node cannot build is a configuration error the operator
		// should see on start-up, not the first time somebody sends a
		// photograph.
		driver, err := n.drivers().Vision(ep.format, compute.VisionDriverConfig{
			Endpoint:   ep.endpoint,
			HTTPClient: modalityEgressClient(ep.label),
			Model:      ep.model,
			Credential: visionCredential(ep.format, ep.apiKey),
			Logger:     n.log,
		})
		if err != nil {
			return fmt.Errorf("read_image: provider %q: %w", ep.label, err)
		}
		cfgs = append(cfgs, compute.VisionConfig{
			Label:       ep.label,
			TrustTier:   ep.trustTier,
			Endpoint:    ep.endpoint,
			Model:       ep.model,
			APIKey:      ep.apiKey,
			Driver:      driver,
			AllowedRoot: n.incomingDir(),
		})
	}
	if err := compute.RegisterVisionBuiltin(builtins, cfgs...); err != nil {
		return fmt.Errorf("register read_image: %w", err)
	}
	if err := n.toolRegistry.Register(compute.VisionToolDef()); err != nil {
		return fmt.Errorf("register read_image tool def: %w", err)
	}
	n.log.Debug("compute: read_image registered",
		"model", eps[0].model, "format", eps[0].format, "via", eps[0].via,
		"chain_len", len(eps))
	return nil
}

// wireAudioTools registers the read_audio (STT) builtin, same
// provider-reference shape as vision. Whisper-compatible multipart
// POST regardless of whether the endpoint is OpenAI, MiniMax, or a
// self-hosted faster-whisper / parakeet sidecar exposing the same
// surface.
func (n *Node) wireAudioTools(builtins *compute.Builtins) error {
	eps := n.resolveAudioEndpoints()
	if len(eps) == 0 {
		return nil
	}
	// Format is per-endpoint, not per-chain: audio matches on two
	// different capabilities, so a chain can legitimately mix a Whisper
	// endpoint with a chat-multimodal one and each must be spoken to in
	// its own protocol.
	cfgs := make([]compute.AudioConfig, 0, len(eps))
	for _, ep := range eps {
		// The driver follows the matched CAPABILITY rather than the
		// provider's `driver` key, which is what lets one chain mix the
		// two shapes. Vision resolves from `driver` because its vendors
		// differ; audio's differ by which capability they advertise.
		driverName := compute.DriverOpenAI
		if ep.matchedCap == compute.CapabilityAudioMultimodal {
			driverName = compute.DriverChatMultimodal
		}
		driver, err := n.drivers().Audio(driverName, compute.AudioDriverConfig{
			Endpoint:   ep.endpoint,
			HTTPClient: modalityEgressClient(ep.label),
			Model:      ep.model,
			Credential: compute.NewBearerCredential(ep.apiKey),
			Logger:     n.log,
		})
		if err != nil {
			return fmt.Errorf("read_audio: provider %q: %w", ep.label, err)
		}
		cfgs = append(cfgs, compute.AudioConfig{
			Label:       ep.label,
			TrustTier:   ep.trustTier,
			Endpoint:    ep.endpoint,
			Model:       ep.model,
			APIKey:      ep.apiKey,
			Driver:      driver,
			AllowedRoot: n.incomingDir(),
		})
	}
	if err := compute.RegisterAudioBuiltin(builtins, cfgs...); err != nil {
		return fmt.Errorf("register read_audio: %w", err)
	}
	if err := n.toolRegistry.Register(compute.AudioToolDef()); err != nil {
		return fmt.Errorf("register read_audio tool def: %w", err)
	}
	n.log.Debug("compute: read_audio registered",
		"model", cfgs[0].Model, "via", eps[0].via,
		"chain_len", len(eps))
	return nil
}

// wireSoulTools registers the soul self-tuning builtins. Default-deny
// via the seedDefault rules — operators open per-scope so only the
// owner's chat can mutate the agent's identity. soul_get is also
// default-deny by virtue of being in the soul_* namespace;
// tighten/loosen per scope as needed.
func (n *Node) wireSoulTools(builtins *compute.Builtins) error {
	if n.soulAdjuster == nil {
		return nil
	}
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
	return nil
}

// wirePDFTools registers the read_pdf builtin, capability="pdf". Same
// chat-completions shape as audio-multimodal — content part
// {type:"file"} with base64 PDF data. OpenRouter is the easy on-ramp;
// Anthropic native PDF and Gemini PDF can land as additional formats.
func (n *Node) wirePDFTools(builtins *compute.Builtins) error {
	eps := n.resolvePDFEndpoints()
	if len(eps) == 0 {
		return nil
	}
	cfgs := make([]compute.PDFConfig, 0, len(eps))
	for _, ep := range eps {
		cfgs = append(cfgs, compute.PDFConfig{
			Label:       ep.label,
			TrustTier:   ep.trustTier,
			Endpoint:    ep.endpoint,
			Model:       ep.model,
			APIKey:      ep.apiKey,
			AllowedRoot: n.incomingDir(),
		})
	}
	if err := compute.RegisterPDFBuiltin(builtins, cfgs...); err != nil {
		return fmt.Errorf("register read_pdf: %w", err)
	}
	if err := n.toolRegistry.Register(compute.PDFToolDef()); err != nil {
		return fmt.Errorf("register read_pdf tool def: %w", err)
	}
	n.log.Debug("compute: read_pdf registered",
		"model", eps[0].model, "via", eps[0].via, "chain_len", len(eps))
	return nil
}

// visionCredential picks how a vendor wants its key presented.
//
// Google authenticates generateContent with a URL query parameter,
// everyone else with a header. Both are expressed as a Credential so
// no provider's auth lives somewhere the others' does not.
func visionCredential(driver, apiKey string) compute.Credential {
	if apiKey == "" {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case gemini.DriverName:
		return compute.NewQueryCredential("key", apiKey)
	case compute.DriverAnthropic:
		// Anthropic wants its own header and a pinned API version;
		// a bearer token is silently ignored.
		return compute.NewHeaderCredential("x-api-key", apiKey)
	default:
		return compute.NewBearerCredential(apiKey)
	}
}

// wireSkillViewTool installs skill_view, the level-1 and level-2 half
// of progressive disclosure.
//
// Not registered when there is no skill registry: a node with no
// skills would advertise a tool that can only ever say "no skill named
// that is installed", which teaches the model to stop asking.
func (n *Node) wireSkillViewTool(builtins *compute.Builtins) error {
	docs := n.skillDocs()
	if docs == nil {
		return nil
	}
	if err := compute.RegisterSkillViewBuiltin(builtins, compute.SkillViewConfig{Docs: docs}); err != nil {
		return fmt.Errorf("register skill_view: %w", err)
	}
	if err := n.toolRegistry.Register(compute.SkillViewToolDef()); err != nil {
		return fmt.Errorf("register skill_view tool def: %w", err)
	}
	return nil
}

// incomingDir is where inbound attachments land and the only place
// the read_* builtins will open a path.
//
// One accessor for both halves. The channel writes here and the
// builtins read here; two settings that had to agree would eventually
// not, and the failure mode is a file sitting somewhere the agent is
// not allowed to look at it — which reads as "the model cannot see
// images" rather than as a path mismatch.
func (n *Node) incomingDir() string {
	if d := n.cfg.Gateway.IncomingDir; d != "" {
		return d
	}
	return compute.DefaultIncomingDir
}
