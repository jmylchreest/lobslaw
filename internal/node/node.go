package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
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

	// Compute-function stack. Non-nil iff FunctionCompute is enabled.
	toolRegistry *compute.Registry
	hooksDisp    *hooks.Dispatcher
	policyEngine *policy.Engine
	resolver     *compute.Resolver
	llmProvider  compute.LLMProvider
	executor     *compute.Executor
	agent        *compute.Agent

	shutdownOnce chan struct{}
}

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

	// Optional UDP broadcast. Runs until ctx is cancelled.
	if n.broadcaster != nil {
		go func() {
			if err := n.broadcaster.Start(ctx); err != nil {
				n.log.Warn("broadcast exited", "err", err)
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
			Logger:   n.log,
		})
		if err != nil {
			return fmt.Errorf("agent: %w", err)
		}
		n.agent = a
	}

	n.log.Info("compute stack wired",
		"has_policy_engine", n.policyEngine != nil,
		"providers", len(n.cfg.Compute.Providers),
		"chains", len(n.cfg.Compute.Chains),
		"has_agent", n.agent != nil,
	)
	return nil
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
