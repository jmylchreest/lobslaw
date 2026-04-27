package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/mcp"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/plan"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/singleton"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/internal/storage"
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

	// MemorySession is the [memory.session] sub-block — controls
	// the auto-seeded session retention pruner.
	MemorySession config.SessionConfig

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

	policySvc     *policy.Service
	memorySvc     *memory.Service
	planSvc       *plan.Service
	storageSvc    *storage.Service
	storageMgr    *storage.Manager
	skillRegistry *skills.Registry
	// Soul is held behind an atomic pointer so the config watcher
	// can hot-swap SOUL.md edits without racing readers. Callers
	// go through the Soul() accessor; never read soul directly.
	soul         atomic.Pointer[soul.Soul]
	soulAdjuster *soul.Adjuster
	soulTuneSvc  *memory.SoulTuneService
	skillAdapter *skills.AgentAdapter

	// Compute-function stack. Non-nil iff FunctionCompute is enabled.
	toolRegistry     *compute.Registry
	hooksDisp        *hooks.Dispatcher
	policyEngine     *policy.Engine
	resolver         *compute.Resolver
	llmProvider      compute.LLMProvider
	executor         *compute.Executor
	agent            *compute.Agent
	embedder         compute.EmbeddingProvider
	roleMap          *compute.RoleMap
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
	promptRegistry  *gateway.PromptRegistry

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
		grpc.ChainUnaryInterceptor(
			grpcinterceptors.RequestID(log),
			grpcinterceptors.Recovery(log),
		),
		grpc.ChainStreamInterceptor(
			grpcinterceptors.RequestIDStream(log),
			grpcinterceptors.RecoveryStream(log),
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
func (n *Node) Start(ctx context.Context) error {
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

	// Config hot-reload watcher. Watches SOUL.md for edits and swaps
	// the atomic Soul pointer — subsystems reading via n.Soul() see
	// the new baseline on their next Load. config.toml watching is
	// follow-up work (it requires coordinating subsystem-specific
	// swap handlers).
	if n.cfg.SoulPath != "" {
		go n.runSoulWatcher(ctx)
	}

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
		}
	}

	// Skill registry watcher: fsnotify on the skills storage
	// mount so drop-in manifests are auto-discovered. Gated on
	// both the registry and a configured storage label —
	// deployments without skills just skip.
	if n.skillRegistry != nil && n.storageMgr != nil && n.cfg.Skills.StorageLabel != "" {
		label := n.cfg.Skills.StorageLabel
		if err := n.skillRegistry.Watch(ctx, n.storageMgr, label); err != nil {
			n.log.Warn("skills: watcher failed to start",
				"label", label, "err", err)
		} else {
			n.log.Info("skills: watcher started", "label", label)
		}
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
	if has(cfg.Functions, types.FunctionMemory) && !has(cfg.Functions, types.FunctionStorage) {
		return errors.New("node.Config: memory function requires storage function on the same node")
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
// handlers. Defaults to ChannelSecretResolver / the main APIKeyResolver
// so tests can inject canned secrets without touching env:/file:/kms:
// resolution.
func (n *Node) resolveChannelSecret(ref string) (string, error) {
	if n.cfg.ChannelSecretResolver != nil {
		return n.cfg.ChannelSecretResolver(ref)
	}
	return n.resolveAPIKey(ref)
}

// resolveAPIKey looks up a provider's APIKeyRef via the configured
// resolver, falling back to config.ResolveSecret for the default
// "env:/file:/kms:" reference scheme. Empty ref means "no auth",
// which is legitimate for local providers like Ollama.
func (n *Node) resolveAPIKey(ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if n.cfg.APIKeyResolver != nil {
		return n.cfg.APIKeyResolver(ref)
	}
	return config.ResolveSecret(ref)
}
