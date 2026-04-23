package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/raft"
	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/plan"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/internal/storage"
	storagelocal "github.com/jmylchreest/lobslaw/internal/storage/local"
	storagenfs "github.com/jmylchreest/lobslaw/internal/storage/nfs"
	storagerclone "github.com/jmylchreest/lobslaw/internal/storage/rclone"
	"github.com/jmylchreest/lobslaw/pkg/auth"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
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

	// Bootstrap should be true on exactly one node of a new cluster on
	// its first-ever start. On restarts it's ignored via
	// raft.ErrCantBootstrap.
	Bootstrap bool

	// SnapshotTarget is a reference like "storage:r2-backup" to a
	// target that receives periodic Raft snapshots. Required when the
	// Memory function is enabled unless SeedNodes is non-empty
	// (meaning this node will join a multi-node cluster where durability
	// comes from replication). See lobslaw-single-node-durability.
	SnapshotTarget string

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

	policySvc *policy.Service
	memorySvc *memory.Service
	planSvc      *plan.Service
	storageSvc   *storage.Service
	storageMgr   *storage.Manager
	skillRegistry *skills.Registry
	// Soul is held behind an atomic pointer so the config watcher
	// can hot-swap SOUL.md edits without racing readers. Callers
	// go through the Soul() accessor; never read soul directly.
	soul         atomic.Pointer[soul.Soul]
	skillAdapter *skills.AgentAdapter

	// Compute-function stack. Non-nil iff FunctionCompute is enabled.
	toolRegistry *compute.Registry
	hooksDisp    *hooks.Dispatcher
	policyEngine *policy.Engine
	resolver     *compute.Resolver
	llmProvider  compute.LLMProvider
	executor     *compute.Executor
	agent        *compute.Agent

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

	// Audit log coordinator and gRPC surface. Present whenever at
	// least one sink is enabled; nil otherwise (operator explicitly
	// turned both off).
	auditLog *audit.AuditLog
	auditSvc *audit.Service

	shutdownOnce chan struct{}
}

// JWTValidator returns the configured JWT validator or nil when
// no Auth method is enabled. Channels (REST, Telegram) consume
// this at startup to decide whether to require auth.
func (n *Node) JWTValidator() *auth.Validator { return n.jwtValidator }

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
		shutdownOnce: make(chan struct{}),
	}
	n.soul.Store(loadedSoul)

	// Wire the Raft stack iff we're running memory or policy. Services
	// that need it (NodeService's AddMember, the minimal PolicyService)
	// register after Raft is up so we don't RegisterService twice on
	// the same gRPC server.
	if needsRaft(cfg.Functions) {
		if err := n.wireRaft(advertise); err != nil {
			n.closePartial()
			return nil, err
		}
		n.policySvc = policy.NewService(n.raft)
		lobslawv1.RegisterPolicyServiceServer(server, n.policySvc)

		n.memorySvc = memory.NewService(n.raft, n.store, log)
		lobslawv1.RegisterMemoryServiceServer(server, n.memorySvc)

		// PlanService needs the Raft stack; registration is independent
		// of compute/gateway. Reads hit the same local store every
		// other service uses.
		n.planSvc = plan.NewService(n.raft, 0)
		lobslawv1.RegisterPlanServiceServer(server, n.planSvc)

		// Scheduler also needs Raft. Constructed here so its FSM-change
		// callback is wired before any subsequent Apply could land.
		// Start is called from Run (blocks on the tick loop until ctx
		// cancel), so boot-time New just builds the struct.
		handlers := scheduler.NewHandlerRegistry()
		sched, err := scheduler.NewScheduler(scheduler.Config{
			NodeID: cfg.NodeID,
			Logger: log,
		}, n.raft, handlers)
		if err != nil {
			n.closePartial()
			return nil, fmt.Errorf("scheduler: %w", err)
		}
		n.scheduler = sched

		// Dream/REM handler. Registered once memorySvc + scheduler are
		// both known (both live under needsRaft). Operators configure
		// a ScheduledTask with HandlerRef=DreamHandlerRef; the
		// scheduler's CAS-claim model picks one node per fire and the
		// runner's leader-gate makes non-leader wins a soft-skip.
		n.registerDreamHandler()

		// Storage service piggybacks on the Raft stack — every voter
		// serves ListMounts from its local replica, AddMount /
		// RemoveMount propagate via Apply, and the FSM change hook
		// drives a Reconcile on every touch of the storage_mounts
		// bucket.
		if has(cfg.Functions, types.FunctionStorage) {
			mgr := storage.NewManager()
			svc, err := storage.NewService(storage.ServiceConfig{
				Raft:    n.raft,
				Store:   n.store,
				FSM:     n.fsm,
				Manager: mgr,
				Factories: map[string]storage.BackendFactory{
					"local":  storagelocal.Factory,
					"nfs":    storagenfs.Factory,
					"rclone": storagerclone.Factory(n.resolveChannelSecret),
				},
				Logger: n.log,
			})
			if err != nil {
				n.closePartial()
				return nil, fmt.Errorf("storage: %w", err)
			}
			n.storageMgr = mgr
			n.storageSvc = svc
			lobslawv1.RegisterStorageServiceServer(server, svc)

			// Wake the reconciler on every replicated change. Initial
			// boot-time reconcile runs from Start so we don't block
			// New on a potentially-slow Apply backlog.
			n.fsm.SetStorageChangeCallback(func() {
				if rerr := svc.Reconcile(context.Background()); rerr != nil {
					n.log.Warn("storage: reconcile failed", "err", rerr)
				}
			})
		}

		// Skill registry + invoker + adapter. Constructed regardless
		// of storage — an operator without a skills mount still has
		// an empty registry (no skills means no skill routing).
		// Registry.Watch is deferred to node.Start so the storage
		// mount can be registered first.
		n.skillRegistry = skills.NewRegistry(n.log)
		skillInvoker, err := skills.NewInvoker(skills.InvokerConfig{
			Registry: n.skillRegistry,
			Storage:  n.storageMgr,
		})
		if err != nil {
			n.closePartial()
			return nil, fmt.Errorf("skills invoker: %w", err)
		}
		adapter, err := skills.NewAgentAdapter(n.skillRegistry, skillInvoker)
		if err != nil {
			n.closePartial()
			return nil, fmt.Errorf("skills adapter: %w", err)
		}
		n.skillAdapter = adapter
	}

	// Audit log + gRPC surface. Runs independently of which functions
	// are enabled — a Compute-only node can still ship a local JSONL
	// of its outbound tool invocations even without Raft. Raft sink
	// is only honoured on nodes that host the Raft stack.
	if err := n.wireAudit(context.Background()); err != nil {
		n.closePartial()
		return nil, fmt.Errorf("audit: %w", err)
	}

	// Wire the Compute stack iff this node runs the Compute function.
	// Depends on the Raft stack above when policy/memory are on the
	// same node — otherwise the Executor's policy engine runs against
	// a local-only store (accepted for single-node deployments where
	// policy function is split off).
	if has(cfg.Functions, types.FunctionCompute) {
		if err := n.wireCompute(); err != nil {
			n.closePartial()
			return nil, fmt.Errorf("compute: %w", err)
		}
	}

	// Auth is independent of functions — every channel handler may
	// need it. Only constructed when the config actually enables a
	// validation method (HS256 secret or JWKS URL); otherwise
	// n.jwtValidator stays nil and channels run in
	// anonymous-with-default-scope mode.
	if cfg.Auth.AllowHS256 || cfg.Auth.JWKSURL != "" {
		authCfg := auth.Config{
			Issuer:     cfg.Auth.Issuer,
			AllowHS256: cfg.Auth.AllowHS256,
			JWKSURL:    cfg.Auth.JWKSURL,
		}
		if cfg.Auth.AllowHS256 {
			secret, err := n.resolveAPIKey(cfg.Auth.JWTSecretRef)
			if err != nil {
				n.closePartial()
				return nil, fmt.Errorf("auth HS256 secret: %w", err)
			}
			authCfg.HS256Secret = secret
		}
		v, err := auth.NewValidator(authCfg)
		if err != nil {
			n.closePartial()
			return nil, fmt.Errorf("auth validator: %w", err)
		}
		n.jwtValidator = v
	}

	// Gateway (channel handlers) — only if the function is enabled
	// AND the config turns it on. Depends on the compute stack for
	// the agent; a gateway-only node with no compute is a misconfig
	// and wireGateway returns an error pointing at the missing
	// function. Runs after Auth so the validator can be wired in.
	if has(cfg.Functions, types.FunctionGateway) && cfg.Gateway.Enabled {
		if err := n.wireGateway(); err != nil {
			n.closePartial()
			return nil, fmt.Errorf("gateway: %w", err)
		}
	}

	// NodeService is registered exactly once, with nil RaftMembership
	// on non-Raft nodes so AddMember returns Unimplemented.
	var raftMembership discovery.RaftMembership
	if n.raft != nil {
		raftMembership = n.raft
	}
	n.discSvc = discovery.NewService(registry, local, log, nil, raftMembership)
	lobslawv1.RegisterNodeServiceServer(server, n.discSvc)

	// Discovery client for seed-list exchange on Start.
	n.discCli = discovery.NewClient(local, registry, n.dialer(), log)

	// Optional UDP broadcast auto-discovery. Constructed here but
	// only started from Start() so cancellation is scoped cleanly.
	if cfg.BroadcastEnabled {
		bc, err := discovery.NewBroadcaster(discovery.BroadcastConfig{
			Address:    cfg.BroadcastAddress,
			ListenAddr: cfg.BroadcastListenAddr,
			Interval:   cfg.BroadcastInterval,
			Local:      local,
			Registry:   registry,
			Logger:     log,
		})
		if err != nil {
			n.closePartial()
			return nil, fmt.Errorf("broadcast setup: %w", err)
		}
		n.broadcaster = bc
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

	// Dial seeds. Failures are non-fatal: even if every seed is down,
	// we keep serving so other nodes can dial us.
	if len(n.cfg.SeedNodes) > 0 {
		if _, err := n.discCli.DialSeeds(ctx, n.cfg.SeedNodes, 5*time.Second); err != nil {
			n.log.Warn("seed-list bootstrap incomplete", "err", err)
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

	// Optional UDP broadcast. Runs until ctx is cancelled.
	if n.broadcaster != nil {
		go func() {
			if err := n.broadcaster.Start(ctx); err != nil {
				n.log.Warn("broadcast exited", "err", err)
			}
		}()
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
	return nil
}

// ListenAddr returns the bound address — useful for tests that let
// the OS pick a port (":0").
func (n *Node) ListenAddr() string {
	return n.listener.Addr().String()
}

// Registry returns the peer registry. Exposed for integration tests
// and future Phase 11 reload plumbing.
func (n *Node) Registry() *discovery.Registry { return n.registry }

// Policy returns the policy service (nil when memory/policy isn't on
// this node). Exposed for test read-paths via GetRule.
func (n *Node) Policy() *policy.Service { return n.policySvc }

// Memory returns the MemoryService implementation. nil on nodes
// without the memory/policy function.
func (n *Node) Memory() *memory.Service { return n.memorySvc }

// Raft returns the underlying RaftNode for tests that need to call
// BootstrapCluster/AddVoter manually (multi-node test setup) or wait
// for leadership.
func (n *Node) Raft() *memory.RaftNode { return n.raft }

// Agent returns the constructed agent loop, or nil when the Compute
// function isn't enabled on this node. Channel handlers (REST,
// Telegram) call this and surface "agent not available" when nil.
func (n *Node) Agent() *compute.Agent { return n.agent }

// ToolRegistry returns the Compute-function tool registry, or nil
// when the function isn't enabled. Callers register tools into it
// at node startup (skills + plugins; manual Register for tests).
func (n *Node) ToolRegistry() *compute.Registry { return n.toolRegistry }

// Resolver returns the provider resolver for the Compute stack.
// Nil when Compute isn't enabled or no providers are configured.
func (n *Node) Resolver() *compute.Resolver { return n.resolver }

// Gateway returns the REST/channel server. Nil when the node didn't
// enable the gateway function or when cfg.Gateway.Enabled is false.
// Tests use this to hit the HTTP endpoints on an ephemeral port;
// main() calls Start implicitly via Node.Start.
func (n *Node) Gateway() *gateway.Server { return n.gatewaySrv }

// Plan returns the PlanService implementation (nil when this node
// doesn't host Raft — PlanService needs Raft-replicated commitments
// and scheduled tasks).
func (n *Node) Plan() *plan.Service { return n.planSvc }

// planServiceOrNil adapts a nullable *plan.Service into the
// gateway.PlanService interface expected by RESTConfig. Returning
// a typed-nil interface here would make the gateway wrongly think
// a service is configured — explicitly check and pass nil instead.
func planServiceOrNil(svc *plan.Service) gateway.PlanService {
	if svc == nil {
		return nil
	}
	return svc
}

// skillDispatcherOrNil mirrors planServiceOrNil: a typed-nil
// *skills.AgentAdapter isn't the same as an untyped nil interface,
// and the agent's "is skill dispatch configured?" check relies on
// the latter.
func skillDispatcherOrNil(a *skills.AgentAdapter) compute.SkillDispatcher {
	if a == nil {
		return nil
	}
	return a
}

// Scheduler returns this node's scheduler. Nil when the node doesn't
// host Raft; otherwise used by the plan + (future) skill layers to
// register HandlerRef → function mappings.
func (n *Node) Scheduler() *scheduler.Scheduler { return n.scheduler }

// Storage returns the local storage Manager. Nil when the node
// doesn't run FunctionStorage. Skill + plugin layers use this to
// Resolve labels + subscribe via the Watcher.
func (n *Node) Storage() *storage.Manager { return n.storageMgr }

// StorageService returns the gRPC-facing StorageService for tests
// that want to drive AddMount/RemoveMount programmatically.
func (n *Node) StorageService() *storage.Service { return n.storageSvc }

// SkillRegistry returns this node's skill registry. Nil when the
// node doesn't host Raft. Tests use this to register skills
// directly; production uses Storage-mounted skills via Watch.
func (n *Node) SkillRegistry() *skills.Registry { return n.skillRegistry }

// Soul returns the loaded SOUL.md as a *soul.Soul. Never nil —
// DefaultSoul fills in when no config path was supplied or the
// file was missing. Callers can safely read Soul().Config without
// nil-checking.
func (n *Node) Soul() *soul.Soul { return n.soul.Load() }

// -- internal helpers --

func (n *Node) wireRaft(advertise string) error {
	store, err := memory.OpenStore(filepath.Join(n.cfg.DataDir, "state.db"), n.cfg.MemoryKey)
	if err != nil {
		return fmt.Errorf("open state.db: %w", err)
	}
	fsm := memory.NewFSM(store)

	transport, err := rafttransport.New(rafttransport.Config{
		LocalAddr: raft.ServerAddress(advertise),
		DialOpts:  []grpc.DialOption{grpc.WithTransportCredentials(n.cfg.Creds.ClientCreds())},
	})
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("rafttransport.New: %w", err)
	}
	transport.Register(n.server)

	rNode, err := memory.NewRaft(memory.RaftConfig{
		NodeID:    n.cfg.NodeID,
		LocalAddr: raft.ServerAddress(advertise),
		DataDir:   n.cfg.DataDir,
		Bootstrap: n.cfg.Bootstrap,
		Transport: transport.RaftTransport(),
	}, fsm)
	if err != nil {
		_ = store.Close()
		return fmt.Errorf("memory.NewRaft: %w", err)
	}

	n.store = store
	n.fsm = fsm
	n.transport = transport
	n.raft = rNode
	return nil
}

// wireAudit constructs the AuditLog coordinator and registers the
// AuditService on the gRPC server. Silently no-ops when both sinks
// are disabled in config — the log object is still created (so
// callers can Append to a nil-sink log without special-casing) but
// no gRPC service is registered to avoid confusing clients with a
// service that will never produce results.
func (n *Node) wireAudit(ctx context.Context) error {
	var sinks []audit.AuditSink

	if n.cfg.Audit.Local.Enabled {
		path := n.cfg.Audit.Local.Path
		if path == "" {
			path = filepath.Join(n.cfg.DataDir, "audit", "audit.jsonl")
		}
		ls, err := audit.NewLocalSink(audit.LocalConfig{
			Path:      path,
			MaxSizeMB: n.cfg.Audit.Local.MaxSizeMB,
			MaxFiles:  n.cfg.Audit.Local.MaxFiles,
		})
		if err != nil {
			return fmt.Errorf("local sink: %w", err)
		}
		sinks = append(sinks, ls)
	}

	if n.cfg.Audit.Raft.Enabled {
		if n.raft == nil || n.store == nil {
			n.log.Warn("audit: raft sink requested but node doesn't host Raft; skipping")
		} else {
			rs, err := audit.NewRaftSink(audit.RaftConfig{
				Raft:  n.raft,
				Store: n.store,
			})
			if err != nil {
				return fmt.Errorf("raft sink: %w", err)
			}
			sinks = append(sinks, rs)
		}
	}

	log, err := audit.NewAuditLog(ctx, audit.Config{
		Sinks:  sinks,
		Logger: n.log,
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}
	n.auditLog = log

	if len(sinks) == 0 {
		n.log.Info("audit: no sinks configured — log is a no-op")
		return nil
	}

	svc, err := audit.NewService(log)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	n.auditSvc = svc
	lobslawv1.RegisterAuditServiceServer(n.server, svc)

	sinkNames := make([]string, len(sinks))
	for i, s := range sinks {
		sinkNames[i] = s.Name()
	}
	n.log.Info("audit wired", "sinks", sinkNames)
	return nil
}

// Audit returns the AuditLog coordinator for the node. Always non-
// nil after New; a zero-sink configuration returns a log that no-ops
// on Append so callers don't need to nil-check.
func (n *Node) Audit() *audit.AuditLog { return n.auditLog }

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

	// LLM provider: injection wins; else build LLMClient from the
	// first provider (simplest "default" behaviour pre-Phase-6-channel).
	switch {
	case n.cfg.LLMProvider != nil:
		n.llmProvider = n.cfg.LLMProvider
	case len(n.cfg.Compute.Providers) > 0:
		first := n.cfg.Compute.Providers[0]
		// Trust-tier guard: refuse to construct a client below the
		// Soul's MinTrustTier floor. ValidateProviderTier is a no-op
		// when the soul hasn't declared a floor (DefaultSoul does
		// not), so deployments without a SOUL.md are unaffected.
		if err := soul.ValidateProviderTier(n.Soul(), soul.ProviderTrustTier{
			Label:     first.Label,
			TrustTier: first.TrustTier,
		}); err != nil {
			return err
		}
		apiKey, err := n.resolveAPIKey(first.APIKeyRef)
		if err != nil {
			return fmt.Errorf("api key for provider %q: %w", first.Label, err)
		}
		client, err := compute.NewLLMClient(compute.LLMClientConfig{
			Endpoint: first.Endpoint,
			APIKey:   apiKey,
			Model:    first.Model,
		})
		if err != nil {
			return fmt.Errorf("llm client for %q: %w", first.Label, err)
		}
		n.llmProvider = client
	}

	// Agent is only constructable with a non-nil Provider. A
	// Compute-enabled node with no providers gets n.agent=nil —
	// REST handler surfaces "provider not configured" at message
	// time rather than blocking boot.
	if n.llmProvider != nil {
		a, err := compute.NewAgent(compute.AgentConfig{
			Provider: n.llmProvider,
			Executor: n.executor,
			Skills:   skillDispatcherOrNil(n.skillAdapter),
			Logger:   n.log,
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

// registerAgentTurnHandlers installs the default task + commitment
// handlers that drive compute.Agent.RunToolCallLoop with the
// scheduler-originated request. Intended to be called once during
// boot; subsequent calls overwrite the prior registration (fine —
// RegisterTask/RegisterCommitment are last-write-wins).
func (n *Node) registerAgentTurnHandlers() {
	_ = n.scheduler.Handlers().RegisterTask(AgentTurnHandlerRef, n.runTaskAsAgentTurn)
	_ = n.scheduler.Handlers().RegisterCommitment(AgentTurnHandlerRef, n.runCommitmentAsAgentTurn)
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
	budget, err := compute.NewTurnBudget(compute.FromConfig(n.cfg.Compute.Budgets))
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	req := compute.ProcessMessageRequest{
		Message: prompt,
		Claims:  n.schedulerClaims(task.CreatedBy),
		TurnID:  fmt.Sprintf("task-%s-%d", task.Id, time.Now().UnixNano()),
		Budget:  budget,
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
	budget, err := compute.NewTurnBudget(compute.FromConfig(n.cfg.Compute.Budgets))
	if err != nil {
		return fmt.Errorf("budget: %w", err)
	}
	req := compute.ProcessMessageRequest{
		Message: prompt,
		Claims:  n.schedulerClaims(c.CreatedFor),
		TurnID:  fmt.Sprintf("commitment-%s-%d", c.Id, time.Now().UnixNano()),
		Budget:  budget,
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
func (n *Node) wireGateway() error {
	if n.agent == nil {
		return fmt.Errorf("gateway requires compute function (no agent wired on this node)")
	}

	n.promptRegistry = gateway.NewPromptRegistry()

	var tg *gateway.TelegramHandler
	for i, ch := range n.cfg.Gateway.Channels {
		switch ch.Type {
		case "telegram":
			h, err := n.buildTelegramHandler(ch)
			if err != nil {
				return fmt.Errorf("gateway.channels[%d] (telegram): %w", i, err)
			}
			tg = h
			n.telegramHandler = h
		case "":
			n.log.Warn("gateway.channels[%d] has empty type; skipping", "index", i)
		default:
			n.log.Warn("gateway.channels: unknown type; skipping",
				"index", i, "type", ch.Type)
		}
	}

	// HTTPPort defaults to 8443 when unset. ListenAddr uses 0.0.0.0
	// unless the operator supplies a specific bind via future config
	// (Phase 6h keeps it simple; a bind-address setting lands with
	// rest-of-cluster operator polish).
	port := n.cfg.Gateway.HTTPPort
	if port == 0 {
		port = 8443
	}
	addr := fmt.Sprintf(":%d", port)

	// Pick a default TLS pair from the first channel that supplies
	// one — Telegram's webhook demands TLS, so if it's configured we
	// want its cert to front the REST surface too. No channel with
	// TLS → plaintext (fine for localhost + reverse-proxy-terminated
	// deployments; operators wanting public HTTPS supply a channel
	// with tls_cert/tls_key).
	var tlsCert, tlsKey string
	for _, ch := range n.cfg.Gateway.Channels {
		if ch.TLSCert != "" && ch.TLSKey != "" {
			tlsCert, tlsKey = ch.TLSCert, ch.TLSKey
			break
		}
	}

	cfg := gateway.RESTConfig{
		Addr:             addr,
		TLSCert:          tlsCert,
		TLSKey:           tlsKey,
		DefaultScope:     n.cfg.Gateway.UnknownUserScope,
		DefaultBudget:    compute.FromConfig(n.cfg.Compute.Budgets),
		JWTValidator:     n.jwtValidator,
		RequireAuth:      n.cfg.Auth.RequireAuth,
		Telegram:         tg,
		Prompts:          n.promptRegistry,
		ConfirmationTTL:  n.cfg.Gateway.ConfirmationTimeout,
		Plan:             planServiceOrNil(n.planSvc),
		Logger:           n.log,
	}

	n.gatewaySrv = gateway.NewServer(cfg, n.agent)
	n.log.Info("gateway wired",
		"http_port", port,
		"tls", tlsCert != "",
		"channels", len(n.cfg.Gateway.Channels),
		"telegram", tg != nil,
		"require_auth", cfg.RequireAuth,
	)
	return nil
}

// buildTelegramHandler resolves bot token + webhook secret from the
// channel config's secret refs and constructs the handler. Secrets
// missing from the environment fail boot loudly — a Telegram channel
// with an empty token is a silent drop of every update.
func (n *Node) buildTelegramHandler(ch config.GatewayChannelConfig) (*gateway.TelegramHandler, error) {
	botToken, err := n.resolveChannelSecret(ch.BotTokenRef)
	if err != nil {
		return nil, fmt.Errorf("bot_token_ref %q: %w", ch.BotTokenRef, err)
	}
	if botToken == "" {
		return nil, fmt.Errorf("bot_token_ref %q resolved to empty — required for Telegram", ch.BotTokenRef)
	}
	webhookSecret, err := n.resolveChannelSecret(ch.SecretTokenRef)
	if err != nil {
		return nil, fmt.Errorf("secret_token_ref %q: %w", ch.SecretTokenRef, err)
	}
	if webhookSecret == "" {
		return nil, fmt.Errorf("secret_token_ref %q resolved to empty — required for Telegram webhook", ch.SecretTokenRef)
	}

	return gateway.NewTelegramHandler(gateway.TelegramConfig{
		BotToken:         botToken,
		WebhookSecret:    webhookSecret,
		UnknownUserScope: n.cfg.Gateway.UnknownUserScope,
		DefaultBudget:    compute.FromConfig(n.cfg.Compute.Budgets),
		Prompts:          n.promptRegistry,
		ConfirmationTTL:  n.cfg.Gateway.ConfirmationTimeout,
		Logger:           n.log,
	}, n.agent)
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
