package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmylchreest/lobslaw/internal/sandbox"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/internal/clawhub"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/mcp"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/notify"
	"github.com/jmylchreest/lobslaw/internal/oauth"
	"github.com/jmylchreest/lobslaw/internal/plan"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/singleton"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/internal/storage"
	"github.com/jmylchreest/lobslaw/internal/trace"
	"github.com/jmylchreest/lobslaw/pkg/auth"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	"github.com/jmylchreest/lobslaw/pkg/rafttransport"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Config is everything node.New needs to stand up a running node.
// Callers (typically cmd/lobslaw/main.go) assemble this from flags +
// config.toml + resolved secrets.
type Config struct {
	NodeID        string
	Functions     []types.NodeFunction
	ListenAddr    string // where to bind the cluster gRPC listener
	AdvertiseAddr string // what peers dial; empty falls back to the bound address
	SeedNodes     []string
	DataDir       string // Raft log + state.db + snapshots/ live here

	// Bootstrap (default true at the config layer) lets this node
	// form a brand-new cluster as a sole voter when it cannot join
	// an existing one via SeedNodes within BootstrapTimeout. False
	// means refuse to start unless we successfully join — the right
	// policy for joiner nodes in production where split-brain is
	// worse than a missing node.
	Bootstrap bool

	// BootstrapTimeout caps the join attempt before falling back to
	// solo-bootstrap (or failing). Zero → 30s.
	BootstrapTimeout time.Duration

	// SnapshotTarget is a reference like "storage:r2-backup" to a
	// target that receives periodic Raft snapshots. Required when the
	// Memory function is enabled unless SeedNodes is non-empty
	// (meaning this node will join a multi-node cluster where durability
	// comes from replication). See lobslaw-single-node-durability.
	SnapshotTarget string

	// MemoryDream is the [memory.dream] sub-block — controls the
	// auto-seeded recurring Dream/REM consolidation pass. Empty
	// values fall back to the seed defaults (enabled, 02:00 daily).
	MemoryDream config.DreamConfig

	// Identity is the [identity] block: the alias map that resolves
	// per-channel user ids to cluster-wide principals. Empty means
	// every channel id is its own principal, which is correct for a
	// deployment where nobody arrives under two names.
	Identity config.IdentityConfig

	// MemorySession is the [memory.session] sub-block — controls
	// the auto-seeded session retention pruner.
	MemorySession config.SessionConfig

	// MemoryWriteApproval stages agent-initiated memory writes for
	// approval. Off by default.
	MemoryWriteApproval bool

	// MemoryPinnedProfileChars / MemoryPinnedNotesChars cap the
	// always-on blocks. Zero takes the defaults.
	MemoryPinnedProfileChars int
	MemoryPinnedNotesChars   int

	// SelfLearningMode is "off" | "propose" | "auto". Empty is off.
	SelfLearningMode string

	// SecretProviderLabels are the configured vault labels, surfaced in
	// the agent's prompt so it knows where a secret belongs. Labels
	// only — nothing here lets the agent read a vault, and no tool
	// does either.
	SecretProviderLabels []string

	// Review trigger thresholds. Zero takes the defaults (10 tool
	// iterations in one turn for skills, 10 conversation turns for
	// memory); negative disables that axis.
	ReviewSkillToolIterations int
	ReviewMemoryTurnInterval  int

	// SelfTaughtHistoryDepth bounds retained prior versions; the two
	// size fields bound one artefact. Zero takes the defaults.
	SelfTaughtHistoryDepth  int
	SelfTaughtMaxFileBytes  int
	SelfTaughtMaxTotalBytes int
	// Curation thresholds in days of disuse. Zero takes the defaults.
	SelfTaughtStaleAfterDays   int
	SelfTaughtArchiveAfterDays int
	// SelfTaughtProposalExpiryDays bounds the review queue. Zero takes
	// the default of 30; negative disables it.
	SelfTaughtProposalExpiryDays int

	// NotifyChannels / NotifySubjects / NotifyInterval are the
	// in-channel review-queue nudge. Both allowlists must be populated
	// or nothing is sent.
	NotifyChannels []string
	NotifySubjects []string
	NotifyInterval time.Duration

	// Trace is turn tracing. Off by default.
	Trace config.TraceConfig

	// SessionGrantTTL bounds a conversation-scoped approval. Zero
	// takes the default of 24h.
	//
	// It exists because the previous bound was the process exiting,
	// which made the lifetime of a security grant a function of deploy
	// cadence rather than of anything anybody decided.
	SessionGrantTTL time.Duration

	// Policy is the [policy] config sub-block — operator-declared
	// [[policy.rules]] entries get seeded at boot.
	Policy config.PolicyConfig

	// UDP broadcast auto-discovery. Leave Enabled=false for production
	// clusters that use seed lists.
	BroadcastEnabled    bool
	BroadcastAddress    string        // e.g. "255.255.255.255:7445"
	BroadcastListenAddr string        // e.g. ":7445"
	BroadcastInterval   time.Duration // 0 = default 30s

	Creds     *mtls.NodeCreds
	MemoryKey crypto.Key // 32-byte key for state.db value encryption

	// AllowEmbeddingModelChange starts the node even though its corpus
	// was embedded by a different model. Set only by
	// --allow-embedding-model-change, for one boot, so
	// `lobslaw memory reembed` can be run — that repair needs a
	// RUNNING node, and refusing unconditionally made the supported
	// migration impossible.
	AllowEmbeddingModelChange bool

	// SandboxPolicyDirs is the resolved policy.d discovery chain, in
	// load order — later overrides earlier on the same tool.
	//
	// Resolved in cmd/lobslaw rather than here because the precedence
	// spans CLI, config and defaults, and only main sees the flags.
	// Empty means no operator policies, which is a supported and common
	// configuration; it is not the same as "use the defaults", because
	// the defaults have already been applied by the time this is set.
	SandboxPolicyDirs []string

	// MTLS carries the certificate PATHS, which Creds does not expose.
	// Enrolment needs them: it reads the cluster CA to hand back to a
	// laptop, and it creates the operator CA beside it.
	MTLS config.MTLSConfig

	// Compute function configuration. Consumed only when
	// types.FunctionCompute is in Functions. Nil / zero values are
	// valid — a Compute-enabled node with no providers simply
	// builds an Agent that can't make LLM calls (useful for tests).
	Compute config.ComputeConfig

	// Hooks is the event-to-hook-configs map for the dispatcher.
	// Typically populated from config.Hooks.
	Hooks config.HooksConfig

	// APIKeyResolver resolves a ProviderConfig.APIKeyRef into a
	// plaintext API key. Nil → config.ResolveSecret is used as the
	// default. Injectable for tests that don't want to touch
	// env/file/secret stores.
	APIKeyResolver func(string) (string, error)

	// LLMProvider overrides the default LLMClient built from the
	// resolver's top provider. When set, the node uses this
	// provider directly (used by integration tests to inject a
	// MockProvider without touching the real HTTP path).
	LLMProvider compute.LLMProvider

	// Auth configures JWT validation for channels. Empty Issuer +
	// AllowHS256=false means no validator is constructed (channels
	// run in anonymous mode unless they explicitly require auth).
	Auth config.AuthConfig

	// SoulPath points at the SOUL.md to load at boot. Empty →
	// soul.DefaultSoul is used. Missing-file also falls back to
	// DefaultSoul (not an error — a node without a SOUL.md runs as
	// a neutral assistant).
	SoulPath string

	// Gateway carries the channel-config shape from config.toml. Only
	// consulted when FunctionGateway is enabled AND cfg.Gateway.Enabled
	// (otherwise the node skips gateway wiring entirely).
	Gateway config.GatewayConfig

	// Audit configures the tamper-evident log. Both sinks can be
	// disabled (no-op log); enabling both gives defence-in-depth
	// where tampering one side fails the cross-sink VerifyChain.
	// Raft sink requires the Raft stack (memory/policy function);
	// config silently drops Raft sink on non-Raft nodes.
	Audit config.AuditConfig

	// Storage carries the config-declared [[storage.mounts]]
	// entries. On leader boot these are seeded into the Raft-
	// backed storage bucket (idempotent — operators can still
	// AddMount at runtime without collision).
	Storage config.StorageConfig

	// Skills carries signing policy + the storage label the
	// Registry's fsnotify watcher subscribes to. Empty
	// StorageLabel → no watcher started; skills registered
	// programmatically still work.
	Skills config.SkillsConfig

	// MCP declares top-level [[mcp.servers]] entries that start at
	// boot alongside any plugin-provided MCP manifests.
	MCP config.MCPConfig

	// Security carries operator knobs for the egress filter and
	// other cross-cutting safety controls. See pkg/config.SecurityConfig
	// for the field-by-field doc.
	Security config.SecurityConfig

	// Users is the operator-declared user list, seeded into
	// BucketUserPrefs at first boot. Carries timezone, language,
	// channel addresses. Solo deployments declare one entry
	// (id="owner"); team deployments add more.
	Users []config.UserConfig

	// Binaries is the operator-declared host-binary catalogue. Each
	// entry mirrors the clawdbot.requires/install pair shape — the
	// agent calls binary_install(name) which walks the declared
	// install array via the same internal/binaries Satisfier
	// pipeline clawhub_install uses. Empty disables binary_install.
	Binaries []config.BinaryConfig

	// Remotes is the operator-declared set of hosts remote_ssh may
	// reach. Empty leaves the tool unregistered — a node with nowhere
	// to dispatch to should not advertise a dispatcher.
	Remotes []config.RemoteConfig

	// DisabledTools are the glob patterns from compute.disabled_tools.
	// A pointer so "unset" and "explicitly empty" stay distinguishable
	// all the way down — see config.ComputeConfig.DisabledTools.
	DisabledTools *[]string

	// APIKeyResolverForChannels overrides the secret-resolver used by
	// channels (Telegram bot token, webhook secret, etc.). Empty means
	// "reuse APIKeyResolver / default env:/file: resolver". Separate
	// field so tests can inject channel-only resolvers without
	// impacting LLM-provider secret resolution.
	ChannelSecretResolver func(string) (string, error)

	Logger *slog.Logger
}

// Node bundles the lifecycle of one cluster member. Constructed via
// New, started via Start, stopped via Shutdown. Shutdown is safe to
// call multiple times.
type Node struct {
	cfg Config
	log *slog.Logger

	listener net.Listener
	server   *grpc.Server

	registry    *discovery.Registry
	discSvc     *discovery.Service
	discCli     *discovery.Client
	broadcaster *discovery.Broadcaster

	// Raft stack — non-nil when memory or policy function enabled.
	store     *memory.Store
	fsm       *memory.FSM
	transport *rafttransport.Transport
	raft      *memory.RaftNode

	policySvc        *policy.Service
	memorySvc        *memory.Service
	credentialSvc    *memory.CredentialService
	userPrefsSvc     *memory.UserPrefsService
	notifySvc        *notify.Service
	oauthTracker     *oauth.Tracker
	oauthProviders   map[string]oauth.ProviderConfig
	clawhubInstaller *clawhub.Installer
	planSvc          *plan.Service
	storageSvc       *storage.Service
	storageMgr       *storage.Manager
	skillRegistry    *skills.Registry
	// jobDrivers maps a generation driver's name to its
	// implementation. The name is embedded in every JobHandle the
	// driver mints, so a handle polled after a crash takeover is
	// routed back to the same driver on a different node.
	jobDriverMu sync.RWMutex
	jobDrivers  map[string]compute.JobDriver
	// Soul is held behind an atomic pointer so the config watcher
	// can hot-swap SOUL.md edits without racing readers. Callers
	// go through the Soul() accessor; never read soul directly.
	soul         atomic.Pointer[soul.Soul]
	soulAdjuster *soul.Adjuster
	soulTuneSvc  *memory.SoulTuneService
	skillAdapter *skills.AgentAdapter

	// Compute-function stack. Non-nil iff FunctionCompute is enabled.
	toolRegistry *compute.Registry
	hooksDisp    *hooks.Dispatcher
	policyEngine *policy.Engine
	resolver     *compute.Resolver
	llmProvider  compute.LLMProvider
	executor     *compute.Executor
	// approvals is shared between the executor (which spends session
	// approvals) and the channels (which record them).
	approvals *compute.SessionApprovals

	// selfTaught holds artefacts the agent authored for itself. Nil
	// when self-learning is off, and nil is the enforcement: there is
	// no write path to guard because there is no store.
	selfTaught *memory.SelfTaughtStore
	// materialiser writes ACTIVE self-taught artefacts into this
	// node's disposable skill cache. Nil when self-learning is off,
	// for the same absence-not-a-flag reason the store is.
	materialiser *skills.Materialiser

	// skillStore is the cluster-store authority for imported skills.
	// Nil on a node with no raft, where nothing can be imported.
	skillStore *memory.SkillStore
	// skillSigningPolicy and skillVerifier are resolved once at boot
	// and reused by every scan, so a reconcile cannot quietly use a
	// different stance from the one reported at startup.
	skillSigningPolicy skills.SigningPolicy
	skillVerifier      *skills.Verifier
	// shadowedSkills remembers which self-taught names already lost to
	// an operator skill, so the reason is logged once per node rather
	// than on every reconcile.
	shadowedSkills sync.Map

	// pinnedStore holds the always-on memory blocks rendered into
	// every system prompt. Nil on a node without raft — there is
	// nowhere to keep them, and a prompt without them is the
	// behaviour that existed before.
	pinnedStore *memory.PinnedStore

	// providerHealth remembers which providers recently failed, so a
	// failover chain skips one in cooldown rather than paying a
	// round-trip to rediscover it every turn. Per-node, not raft: one
	// node behind a broken egress proxy must not convince the cluster
	// that a provider is down.
	providerHealth *compute.ProviderHealth

	// approvalRules mints the permanent policy rule behind an
	// "always" approval. Nil on a node without raft — there is
	// nowhere to record a lasting grant, so the channels hide the
	// button rather than offering one that does nothing.
	approvalRules *policy.ApprovalRules

	// sessionGrants is the replicated backing for "approved for the
	// rest of this conversation". Nil on a node with no local raft,
	// where approvals stay process-local exactly as they were.
	sessionGrants *memory.SessionGrantStore

	// notices appends the review-queue nudge to outbound replies. Nil
	// when self-learning is off or nobody opted in.
	notices *gateway.Notices

	// traces records what each turn did. Nil when tracing is off, and
	// a nil recorder is usable — so instrumented paths record
	// unconditionally rather than branching.
	traces   *trace.Recorder
	agent    *compute.Agent
	embedder compute.EmbeddingProvider
	roleMap  *compute.RoleMap

	// reviewFork decides whether a finished turn taught anything.
	// Nil when self-learning is off — there is no fork to disable
	// because there is nowhere for it to write.
	reviewFork       *compute.ReviewFork
	providerRegistry *compute.ProviderRegistry
	mcpLoader        *mcp.Loader
	webhookHandlers  []*gateway.WebhookHandler
	mountResolver    *compute.MountResolver
	builtinsRegistry *compute.Builtins

	// egressProvider is the in-process smokescreen-backed forward
	// proxy. Constructed early in boot via wireEgress; every later
	// http.Client construction routes through it. Stop is called
	// from closePartial.
	egressProvider *egress.SmokescreenProvider

	// Scheduler fires ScheduledTaskRecord and AgentCommitment records.
	// Runs on any node that has access to the Raft stack (memory or
	// policy function); multi-node clusters rely on its CAS-claim
	// semantics for at-most-one-fires-per-turn.
	scheduler *scheduler.Scheduler

	// jwtValidator is constructed when Auth config enables a
	// validation method (currently HS256; JWKS deferred).
	jwtValidator *auth.Validator

	// Gateway channel layer. Constructed when FunctionGateway is on
	// AND Gateway.Enabled is true. A node with gateway disabled leaves
	// these nil — Gateway() returns nil and Start skips the HTTP server
	// entirely. Keeping them separate from the gRPC server means the
	// cluster control plane stays up even if a user-facing channel
	// misconfigures itself.
	gatewaySrv      *gateway.Server
	telegramHandler *gateway.TelegramHandler
	promptRegistry  gateway.Prompts

	// enrolments queues operator certificate requests; enrolmentSvc
	// is the gRPC face of it, retained so the separate enrolment
	// listener can serve a narrowed view of the same service.
	enrolments   *memory.EnrolmentStore
	enrolmentSvc *enrolmentService

	// promptStore is the raft-backed confirmation store behind
	// promptRegistry, kept separately because the sweeper needs the
	// concrete type. Nil on a gateway node that does not host raft —
	// there, confirmations stay process-local.
	promptStore *memory.PromptStore

	// leaderGate fans raft leadership transitions out to leader-pinned
	// singleton workloads (currently just the telegram long-poller).
	// Constructed when this node hosts raft; nil otherwise.
	leaderGate *singleton.LeaderGate

	// Audit log coordinator and gRPC surface. Present whenever at
	// least one sink is enabled; nil otherwise (operator explicitly
	// turned both off).
	auditLog *audit.AuditLog
	auditSvc *audit.Service

	shutdownOnce chan struct{}

	// Boot-time state that wire stages need to read. Set by New
	// before runWireStages and unused after. Lives on the struct
	// (rather than threaded as args) so stage signatures can be a
	// uniform `func(*Node) error`.
	advertise string
	localInfo types.NodeInfo
}

// JWTValidator returns the configured JWT validator or nil when
// no Auth method is enabled. Channels (REST, Telegram) consume
// this at startup to decide whether to require auth.
// JWTValidator + every other one-line getter live in accessors.go.

// New constructs a Node without starting it. Any construction error
// leaves no partially-initialised subsystems behind — resources
// opened up to the point of failure are closed before the error
// bubbles up.
func New(cfg Config) (*Node, error) {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}

	// Normalise before validating, so every check below — and every
	// gate at wire time — sees one canonical set rather than
	// rediscovering that policy and memory mean the same thing.
	normalised, rewrote := types.NormalizeFunctions(cfg.Functions)
	if len(rewrote) > 0 {
		log.Warn("cluster: deprecated node function; it selects nothing of its own and is treated as \"memory\"",
			"deprecated", rewrote, "functions", normalised)
	}
	cfg.Functions = normalised

	if err := validateConfig(cfg); err != nil {
		return nil, err
	}

	// Soul is loaded before any subsystem that might consume it.
	// LoadOrDefault turns missing-file into DefaultSoul rather than
	// an error — a node without a SOUL.md runs as a neutral
	// assistant. Genuine parse / validation errors propagate so a
	// corrupt SOUL.md doesn't silently downgrade the personality.
	loadedSoul, err := soul.LoadOrDefault(cfg.SoulPath)
	if err != nil {
		return nil, fmt.Errorf("soul: %w", err)
	}
	log.Info("soul loaded",
		"path", cfg.SoulPath,
		"name", loadedSoul.Config.Name,
		"min_trust_tier", loadedSoul.Config.MinTrustTier,
	)

	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen on %q: %w", cfg.ListenAddr, err)
	}

	advertise := cfg.AdvertiseAddr
	if advertise == "" {
		advertise = listener.Addr().String()
	}

	server := grpc.NewServer(
		grpc.Creds(cfg.Creds.ServerCreds()),
		// A skill bundle is capped at DefaultMaxSkillTotalBytes, which
		// is 4 MiB — exactly gRPC's default receive limit. A bundle at
		// the limit plus proto framing would fail with "message too
		// large" instead of the store's message naming the offending
		// file, so the transport ceiling is raised above the one that
		// carries meaning.
		grpc.MaxRecvMsgSize(3*memory.DefaultMaxSkillTotalBytes),
		grpc.MaxSendMsgSize(3*memory.DefaultMaxSkillTotalBytes),
		grpc.ChainUnaryInterceptor(
			grpcinterceptors.RequestID(log),
			grpcinterceptors.Recovery(log),
			// An operator credential administers the cluster; it does
			// not take part in replication. Enforced at the server,
			// because a check on the client is one the attacker
			// controls.
			grpcinterceptors.OperatorNotAPeer(),
		),
		grpc.ChainStreamInterceptor(
			grpcinterceptors.RequestIDStream(log),
			grpcinterceptors.RecoveryStream(log),
			// Raft's transport is streaming, so without this half the
			// guard covers nothing that matters.
			grpcinterceptors.OperatorNotAPeerStream(),
		),
	)

	local := types.NodeInfo{
		ID:         types.NodeID(cfg.NodeID),
		Functions:  cfg.Functions,
		Address:    advertise,
		RaftMember: needsRaft(cfg.Functions),
	}

	registry := discovery.NewRegistry()

	n := &Node{
		cfg:          cfg,
		log:          log,
		listener:     listener,
		server:       server,
		registry:     registry,
		advertise:    advertise,
		localInfo:    local,
		shutdownOnce: make(chan struct{}),
	}
	n.soul.Store(loadedSoul)

	// Before the wire stages, because wireCompute reads n.traces when
	// it constructs the agent. Starting the recorder afterwards would
	// hand the agent a nil one and record nothing — the same
	// registration-order hazard the health tracker and trust floor have
	// on the builtins registry.
	if err := n.startTracing(); err != nil {
		n.closePartial()
		return nil, err
	}

	// Walk the assembly order in wire.go. Each stage gates on a
	// predicate (raft, compute, gateway, etc.) and reads cross-stage
	// state through *Node fields rather than threaded args.
	if err := n.runWireStages(nodeWireStages()); err != nil {
		n.closePartial()
		return nil, err
	}

	return n, nil
}

// Start begins serving gRPC, optionally dials seed nodes, and blocks
// until ctx is cancelled. Cancellation triggers Shutdown.
// gocyclo: 31, just past the configured 30. Start is a sequence of
// guarded startup steps whose branches are almost all "is this
// function enabled"; the shape is flat rather than deeply nested.
// Left as-is because the natural split — per-function start helpers
// — is the same refactor wireCompute needs and belongs with it.
func (n *Node) Start(ctx context.Context) error { //nolint:gocyclo // flat startup sequence; refactor alongside wireCompute
	errCh := make(chan error, 1)
	go func() {
		if err := n.server.Serve(n.listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	n.log.Info("lobslaw node started",
		"node_id", n.cfg.NodeID,
		"listen", n.listener.Addr(),
		"functions", n.cfg.Functions,
	)

	// pprof under build tag `debug` only — see pprof_debug.go.
	n.startPprof(ctx)

	// Reaper for orphaned OAuth flows + synthetic credentials. Per-
	// node loop; the credential-side delete is leader-gated inside.
	n.startReaper(ctx)

	// Dial seeds for peer-registry exchange (Register + GetPeers).
	// Failures are non-fatal: even if every seed is down, we keep
	// serving so other nodes can dial us. Membership-join (raft
	// AddVoter) is a separate flow handled by establishRaftMembership
	// below.
	if len(n.cfg.SeedNodes) > 0 {
		if _, err := n.discCli.DialSeeds(ctx, n.cfg.SeedNodes, 5*time.Second); err != nil {
			n.log.Warn("seed-list bootstrap incomplete", "err", err)
		}
	}

	// Start broadcast BEFORE establishRaftMembership so an empty-state
	// node has a chance to hear ambient announces and dial those peers
	// instead of needing an explicit seed_nodes entry. Listen-only
	// mode at this stage would also work; running both lets us also
	// be discoverable to anyone else that's coming up alongside us.
	if n.broadcaster != nil {
		go func() {
			if err := n.broadcaster.Start(ctx); err != nil {
				n.log.Warn("broadcast exited", "err", err)
			}
		}()
	}

	// Decide raft membership: resume / join / bootstrap. Only runs
	// on raft-hosting nodes (memory or policy function).
	if n.raft != nil {
		if err := n.establishRaftMembership(ctx); err != nil {
			return fmt.Errorf("raft membership: %w", err)
		}
	}

	// Watches SOUL.md for edits and swaps the atomic Soul pointer —
	// subsystems reading via n.Soul() see the new baseline on their
	// next Load.
	//
	// config.toml is watched in cmd/lobslaw instead, because the
	// subsystem-specific swap handlers this used to wait for are not
	// coming: a bound listener and an open mount cannot be swapped.
	// What that watcher does is report which sections an edit changed
	// that need a restart. Soul is different precisely because it HAS
	// a swap point — one atomic pointer, read fresh every turn.
	if n.cfg.SoulPath != "" {
		go n.runSoulWatcher(ctx)
	}

	// Operator policy.d, watched for the same reason SOUL.md is: it is
	// a file the OPERATOR owns and edits, not content the store is
	// authoritative for.
	//
	// That distinction is what makes this consistent with
	// lobslaw-skill-storage-model rather than a return to what it
	// retired. Skill content moved into the store precisely so a
	// filesystem edit stopped being a trust event; an operator's own
	// policy file never lived there, and editing one is exactly the
	// trust event it appears to be.
	n.startSandboxPolicyWatcher(ctx)

	// Scheduler runs for the node lifetime. Exits cleanly on ctx
	// cancel. Only present on Raft-hosting nodes (the construction
	// branch in New gated that).
	if n.scheduler != nil {
		go func() {
			if err := n.scheduler.Run(ctx); err != nil {
				n.log.Warn("scheduler exited", "err", err)
			}
		}()
	}

	n.startPromptSweeper(ctx)
	n.startEnrolmentSweeper(ctx)
	if err := n.startEnrolmentListener(ctx); err != nil {
		return err
	}

	// Initial storage reconcile. Catches the case where the cluster
	// already has storage_mounts entries from prior sessions — the
	// FSM change hook fires only on new writes, not on existing
	// state. Non-fatal if it errors; the FSM hook will retry on the
	// next write and operators can re-issue AddMount to nudge.
	if n.storageSvc != nil {
		if err := n.storageSvc.Reconcile(ctx); err != nil {
			n.log.Warn("storage: initial reconcile failed", "err", err)
		}
	}

	// Seed default policy rules for stdlib builtins. Leader-only
	// and idempotent; followers see them through replication.
	// Runs after a brief leadership wait so single-node bootstrap
	// finishes electing itself before we Apply. Failure is warn-
	// level, not fatal — the node still boots; the first user turn
	// hits default-deny and the operator sees the warning.
	if n.raft != nil {
		if err := n.raft.WaitForLeader(5 * time.Second); err == nil {
			if err := n.seedDefaultPolicyRules(ctx); err != nil {
				n.log.Warn("policy: seed defaults failed", "err", err)
			}
			if err := n.seedStorageMountsFromConfig(ctx); err != nil {
				n.log.Warn("storage: seed from config failed", "err", err)
			}
			if err := n.seedDreamTask(ctx); err != nil {
				n.log.Warn("memory: seed dream task failed", "err", err)
			}
			if err := n.seedSessionPruneTask(ctx); err != nil {
				n.log.Warn("memory: seed session prune task failed", "err", err)
			}
			if err := n.seedUserPrefsFromConfig(ctx); err != nil {
				n.log.Warn("user_prefs: seed from config failed", "err", err)
			}
		}
	}

	// After the seeds, so config-supplied rules are audited too, and
	// after every RegisterCondition — a rule naming a condition this
	// build cannot evaluate does not do what it says, and warn level
	// is where that used to hide.
	if n.policyEngine != nil {
		n.policyEngine.LogUnevaluableRules()
	}

	// The self-taught cache. Started before the storage watcher so
	// an agent-authored skill and an operator one of the same name
	// are both candidates by the time the first turn is served —
	// otherwise the winner would depend on scan order for the first
	// few seconds of uptime, which is exactly the kind of
	// nondeterminism tier-first precedence exists to remove.
	// The lifecycle pass, leader-gated — the opposite of the
	// materialiser below, which every node must run because a cache is
	// per-node and a lifecycle is not.
	n.startCurator(ctx)
	n.startGrantSweeper(ctx)

	if err := n.startMaterialiser(ctx); err != nil {
		n.log.Error("skills: materialiser failed to start", "err", err)
	}

	// After the materialiser, which builds the cache root both loaders
	// write into.
	if err := n.startSkillStoreLoader(ctx); err != nil {
		return fmt.Errorf("skills: %w", err)
	}

	// The skills mount, now an IMPORT source rather than a live one.
	// It feeds the store; the store is what the registry loads from.
	// Started after the store loader so the first import has somewhere
	// to land and something to materialise it.
	if err := n.startSkillMountImport(ctx); err != nil {
		n.log.Warn("skills: mount import not started", "err", err)
	}

	// MCP servers from top-level [mcp.servers] config. Plugin
	// manifests can also declare servers; the loader dedupes by
	// name (first-registered wins). Failures per server are
	// isolated — a misconfigured integration doesn't block boot.
	n.log.Info("mcp: wireup", "configured_servers", len(n.cfg.MCP.Servers))
	if len(n.cfg.MCP.Servers) > 0 {
		if err := n.startMCPFromConfig(ctx); err != nil {
			n.log.Warn("mcp: direct servers failed to start", "err", err)
		}
		n.registerMCPToolsWithCompute()
	}

	// Gateway HTTP server, when wired. Runs until ctx is cancelled;
	// a failure to bind surfaces through errCh so we fail the whole
	// node (a gateway-enabled node that couldn't bind its channel
	// surface isn't useful — better to crash + let the supervisor
	// restart than silently serve only gRPC).
	if n.gatewaySrv != nil {
		go func() {
			if err := n.gatewaySrv.Start(ctx); err != nil {
				errCh <- fmt.Errorf("gateway serve: %w", err)
			}
		}()
	}

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		n.log.Info("shutdown signal received")
		return n.Shutdown(context.Background())
	}
}

// Shutdown stops the gRPC server (graceful if possible, force if it
// hangs), shuts Raft down, closes the store. Safe to call more than
// once — subsequent calls are no-ops.
func (n *Node) Shutdown(ctx context.Context) error {
	select {
	case <-n.shutdownOnce:
		return nil
	default:
	}
	close(n.shutdownOnce)

	// Drained first, before the gRPC stop. A shutdown that discards the
	// buffer loses precisely the spans from the turn that was in flight
	// when the operator hit stop, which is usually the one they wanted.
	n.stopTracing()

	// Graceful gRPC shutdown with a hard timeout.
	stopped := make(chan struct{})
	go func() {
		n.server.GracefulStop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		n.log.Warn("gRPC graceful-stop timed out; forcing")
		n.server.Stop()
	}

	if n.storageMgr != nil {
		for _, err := range n.storageMgr.StopAll(ctx) {
			n.log.Warn("storage shutdown", "err", err)
		}
	}
	if n.auditLog != nil {
		if err := n.auditLog.Close(); err != nil {
			n.log.Warn("audit log close", "err", err)
		}
	}
	if n.raft != nil {
		if err := n.raft.Shutdown(); err != nil {
			n.log.Warn("raft shutdown", "err", err)
		}
	}
	if n.store != nil {
		if err := n.store.Close(); err != nil {
			n.log.Warn("store close", "err", err)
		}
	}
	if n.egressProvider != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := n.egressProvider.Stop(stopCtx); err != nil {
			n.log.Warn("egress proxy shutdown", "err", err)
		}
	}
	return nil
}

// ListenAddr returns the bound address — useful for tests that let
// -- internal helpers --

// ReloadableSection names what NodeService.Reload knows how to
// dispatch. Kept as named constants so the CLI + docs don't drift
// from the switch below.
const (
	ReloadSoul   = "soul"
	ReloadEgress = "egress"
)

// allReloadable lists sections reloaded when the caller passes an
// empty section list.
var allReloadable = []string{ReloadSoul, ReloadEgress}

// reloadSections is the ReloadFunc handed to discovery.Service. It
// dispatches per-section: known sections reload in place; unknown
// sections land in the errors map; sections that this node can't
// hot-reload (none today, but the plumbing is here) go into
// restartNeeded so the caller knows a full restart is required.
//
// Empty `sections` means "reload everything reloadable on this
// node." Reload is intentionally per-node — config.toml lives on
// disk per node, so cluster-wide reload is the caller orchestrating
// a Reload RPC against every peer.
func (n *Node) reloadSections(_ context.Context, sections []string) (reloaded, restartNeeded []string, errs map[string]string) {
	errs = map[string]string{}
	if len(sections) == 0 {
		sections = allReloadable
	}
	for _, section := range sections {
		switch section {
		case ReloadSoul:
			if n.cfg.SoulPath == "" {
				errs[section] = "no SoulPath configured; nothing to reload"
				continue
			}
			loaded, err := soul.LoadOrDefault(n.cfg.SoulPath)
			if err != nil {
				errs[section] = err.Error()
				continue
			}
			n.soul.Store(loaded)
			reloaded = append(reloaded, section)
			n.log.Info("reload: soul replaced",
				"name", loaded.Config.Name,
				"path", n.cfg.SoulPath)
		case ReloadEgress:
			if err := n.refreshEgressACL(); err != nil {
				errs[section] = err.Error()
				continue
			}
			reloaded = append(reloaded, section)
		default:
			errs[section] = "unknown section"
		}
	}
	return reloaded, restartNeeded, errs
}

// runSoulWatcher blocks until ctx is cancelled, reloading SOUL.md
// on edits. Parse / validation errors are logged and the live Soul
// pointer is left unchanged — a corrupt edit does not downgrade
// personality to DefaultSoul mid-session.
func (n *Node) runSoulWatcher(ctx context.Context) {
	err := config.Watch(ctx, config.WatchOptions{
		Paths:  []string{n.cfg.SoulPath},
		Logger: n.log,
	}, func(_ []fsnotify.Event) {
		loaded, err := soul.LoadOrDefault(n.cfg.SoulPath)
		if err != nil {
			n.log.Warn("soul hot-reload: parse failed; keeping previous",
				"path", n.cfg.SoulPath, "err", err)
			return
		}
		n.soul.Store(loaded)
		n.log.Info("soul hot-reloaded",
			"path", n.cfg.SoulPath,
			"name", loaded.Config.Name,
			"min_trust_tier", loaded.Config.MinTrustTier,
		)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		n.log.Warn("soul watcher exited", "err", err)
	}
}

func (n *Node) dialer() discovery.Dialer {
	return func(ctx context.Context, addr string) (*grpc.ClientConn, error) {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(n.cfg.Creds.ClientCreds()))
	}
}

// closePartial runs during construction when we've opened some
// resources but hit an error. Best-effort cleanup; errors swallowed
// because we're already returning a failure.
func (n *Node) closePartial() {
	n.stopTracing()
	if n.store != nil {
		_ = n.store.Close()
	}
	if n.listener != nil {
		_ = n.listener.Close()
	}
}

func validateConfig(cfg Config) error {
	if cfg.NodeID == "" {
		return errors.New("node.Config: NodeID required")
	}
	if cfg.ListenAddr == "" {
		return errors.New("node.Config: ListenAddr required")
	}
	if cfg.Creds == nil {
		return errors.New("node.Config: Creds required (run `lobslaw cluster sign-node` first)")
	}
	if cfg.Creds.NodeID != cfg.NodeID {
		return fmt.Errorf("node.Config: cert was signed for %q but this host resolves as %q — re-run `lobslaw cluster sign-node` on this host (or set LOBSLAW_NODE_ID to override)", cfg.Creds.NodeID, cfg.NodeID)
	}
	if needsRaft(cfg.Functions) {
		if cfg.DataDir == "" {
			return errors.New("node.Config: DataDir required when memory or policy function is enabled")
		}
		var zero crypto.Key
		if cfg.MemoryKey == zero {
			return errors.New("node.Config: MemoryKey required when memory or policy function is enabled")
		}
	}
	// Durability check: a memory-enabled node running alone with no
	// external snapshot target is one disk failure away from total
	// amnesia. Require EITHER a snapshot target OR seed nodes (which
	// mean this node joins a multi-node cluster where replication
	// provides durability).
	if has(cfg.Functions, types.FunctionMemory) && cfg.SnapshotTarget == "" && len(cfg.SeedNodes) == 0 {
		return errors.New("node.Config: memory-enabled nodes without seeds must configure memory.snapshot.target " +
			"(a single-node cluster with no off-cluster backup risks total data loss on disk failure)")
	}
	return nil
}

func needsRaft(fns []types.NodeFunction) bool {
	return has(fns, types.FunctionMemory) || has(fns, types.FunctionPolicy)
}

func has(fns []types.NodeFunction, target types.NodeFunction) bool {
	for _, f := range fns {
		if f == target {
			return true
		}
	}
	return false
}

// wireCompute constructs the Compute-function stack: Registry,
// Executor (with policy engine + hooks + sandbox), Resolver, LLM
// provider, and Agent. Runs after the Raft stack is up because
// policy.Engine needs memory.Store for rule reads.
//
// The LLM provider is either the one injected via Config.LLMProvider
// (tests, mock deployments) or a real LLMClient built from the
// resolver's first provider. A Compute-enabled node with no providers
// configured gets an Agent without a provider — calling it yields
// ErrNoLLMProvider at RunToolCallLoop time, which is fine: the node
// still accepts messages but reports the config gap.
func (n *Node) soulProvider() *types.SoulConfig {
	s := n.Soul()
	if s == nil {
		return nil
	}
	cfg := s.Config
	return &cfg
}

// parseUserScopes converts the TOML string-keyed user_scopes map
// into the int64-keyed shape the Telegram handler expects. Empty
// input → nil (handler treats that as "no explicit mappings").
func parseUserScopes(raw map[string]string) (map[int64]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[int64]string, len(raw))
	for k, v := range raw {
		id, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("user_scopes key %q: not a valid int64: %w", k, err)
		}
		out[id] = v
	}
	return out, nil
}

// resolveChannelSecret is the secret-ref resolver used by channel
// handlers, MCP servers and storage mounts. Defaults to
// ChannelSecretResolver / the main APIKeyResolver so tests can inject
// canned secrets without touching real resolution.
func (n *Node) resolveChannelSecret(ref string) (string, error) {
	if n.cfg.ChannelSecretResolver != nil {
		return n.cfg.ChannelSecretResolver(ref)
	}
	return n.resolveAPIKey(ref)
}

// resolveAPIKey looks up a provider's APIKeyRef via the configured
// resolver.
//
// cmd/lobslaw injects one built from [[secrets.providers]], so a ref
// may name a vault. The fallback is the bootstrap scheme set —
// env: and file: — which is what a node constructed directly in a test
// gets, and what every deployment got before providers existed.
//
// Empty ref means "no auth", which is legitimate for local providers
// like Ollama.
func (n *Node) resolveAPIKey(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if n.cfg.APIKeyResolver != nil {
		return n.cfg.APIKeyResolver(ref)
	}
	return config.ResolveSecret(ref)
}

// resolveOAuthProviders walks cfg.Security.OAuth, resolves each
// entry's ClientIDRef + ClientSecretRef via the same secret resolver
// used for API keys, and returns the runtime ProviderConfig map the
// credentials builtins consume. Known names ("google", "github")
// inherit endpoint defaults from internal/oauth — operators only
// supply ClientID(/Secret).
func (n *Node) resolveOAuthProviders() (map[string]oauth.ProviderConfig, error) {
	if len(n.cfg.Security.OAuth) == 0 {
		return nil, nil
	}
	out := make(map[string]oauth.ProviderConfig, len(n.cfg.Security.OAuth))
	for name, raw := range n.cfg.Security.OAuth {
		base := defaultOAuthProvider(name)
		if raw.DeviceAuthEndpoint != "" {
			base.DeviceAuthEndpoint = raw.DeviceAuthEndpoint
		}
		if raw.TokenEndpoint != "" {
			base.TokenEndpoint = raw.TokenEndpoint
		}
		if len(raw.DefaultScopes) > 0 {
			base.DefaultScopes = append([]string(nil), raw.DefaultScopes...)
		}
		if raw.SubjectClaim != "" {
			base.SubjectClaim = raw.SubjectClaim
		}
		clientID, err := n.resolveAPIKey(raw.ClientIDRef)
		if err != nil {
			return nil, fmt.Errorf("oauth %q client_id_ref: %w", name, err)
		}
		base.ClientID = clientID
		if raw.ClientSecretRef != "" {
			secret, err := n.resolveAPIKey(raw.ClientSecretRef)
			if err != nil {
				return nil, fmt.Errorf("oauth %q client_secret_ref: %w", name, err)
			}
			base.ClientSecret = secret
		}
		base.Name = name
		if err := base.Validate(); err != nil {
			return nil, fmt.Errorf("oauth %q: %w", name, err)
		}
		out[name] = base
	}
	return out, nil
}

// resolveUserTimezone returns the IANA zone for the given user_id,
// falling back to the cluster default and finally UTC. Used by the
// agent to carry the user's timezone on the turn identity + render
// time output naturally for the user.
func (n *Node) resolveUserTimezone(userID string) string {
	clusterDefault := strings.TrimSpace(n.cfg.Gateway.DefaultTimezone)
	if clusterDefault == "" {
		clusterDefault = "UTC"
	}
	if userID == "" || n.userPrefsSvc == nil {
		return clusterDefault
	}
	prefs, err := n.userPrefsSvc.Get(context.Background(), userID)
	if err != nil || prefs == nil {
		return clusterDefault
	}
	if tz := strings.TrimSpace(prefs.Timezone); tz != "" {
		return tz
	}
	return clusterDefault
}

// resolveUserRoles returns the roles the operator declared for the
// person behind a channel user id, for channels that carry no JWT to
// assert them with.
//
// The lookup is by principal, not by the raw channel id, because the
// raw id is per-channel and the declaration is per-person: [[user]]
// says id = "alice" while Telegram says "tg-@alice". Resolving first
// means one declaration covers every channel that person arrives on,
// and it covers them through the same alias map that already decides
// what they own — a role that disagreed with ownership would be the
// worst of both.
//
// Roles come from config rather than BucketUserPrefs deliberately.
// Prefs are runtime-editable through a builtin, so a model that
// talked its way into a prefs write could grant itself a role; the
// config file is only writable by whoever runs the node.
func (n *Node) resolveUserRoles(userID string) []string {
	if strings.TrimSpace(userID) == "" || len(n.cfg.Users) == 0 {
		return nil
	}
	principal := n.identityResolver().Resolve(userID)
	if principal.IsZero() {
		return nil
	}
	for _, u := range n.cfg.Users {
		if len(u.Roles) == 0 {
			continue
		}
		if identity.User(strings.TrimSpace(u.ID)) != principal {
			continue
		}
		out := make([]string, 0, len(u.Roles))
		for _, r := range u.Roles {
			if r = strings.TrimSpace(r); r != "" {
				out = append(out, r)
			}
		}
		return out
	}
	return nil
}

func defaultOAuthProvider(name string) oauth.ProviderConfig {
	switch name {
	case "google":
		return oauth.Google()
	case "github":
		return oauth.GitHub()
	case "microsoft":
		return oauth.Microsoft()
	case "gitlab":
		return oauth.GitLab()
	default:
		return oauth.ProviderConfig{Name: name}
	}
}

// startSandboxPolicyWatcher reloads operator policy.d on edit.
//
// Watcher.Start performs its own synchronous initial load before
// returning, which repeats the one applyOperatorPolicies already did
// during wiring. That is deliberate rather than tolerated: the second
// load is what seeds the watcher's knownTools set, and knownTools is
// what makes a DELETED policy file mean "hand this tool back to its
// default" instead of leaving the last-loaded policy in force forever.
// The cost is one directory read at boot.
//
// Failure is not fatal. The policies applied during wiring are already
// in force; losing the watcher costs hot-reload, not confinement.
func (n *Node) startSandboxPolicyWatcher(ctx context.Context) {
	if n.toolRegistry == nil || len(n.cfg.SandboxPolicyDirs) == 0 {
		return
	}
	w := sandbox.NewWatcherMulti(
		n.cfg.SandboxPolicyDirs, n.toolRegistry, sandbox.LoadOptions{}, 0)
	if err := w.Start(ctx); err != nil {
		n.log.Warn("sandbox: policy.d watcher did not start; edits will need a restart",
			"dirs", n.cfg.SandboxPolicyDirs, "err", err)
		return
	}
	n.log.Info("sandbox: watching operator policy.d for changes",
		"dirs", n.cfg.SandboxPolicyDirs)
}
