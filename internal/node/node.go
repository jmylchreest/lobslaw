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

	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
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

	Creds     *mtls.NodeCreds
	MemoryKey crypto.Key // 32-byte key for state.db value encryption

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

	registry *discovery.Registry
	discSvc  *discovery.Service
	discCli  *discovery.Client

	// Raft stack — non-nil when memory or policy function enabled.
	store     *memory.Store
	fsm       *memory.FSM
	transport *rafttransport.Transport
	raft      *memory.RaftNode

	policySvc *policy.Service

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
	discSvc := discovery.NewService(registry, local, log, nil)
	lobslawv1.RegisterNodeServiceServer(server, discSvc)

	n := &Node{
		cfg:          cfg,
		log:          log,
		listener:     listener,
		server:       server,
		registry:     registry,
		discSvc:      discSvc,
		shutdownOnce: make(chan struct{}),
	}

	// Wire the Raft stack iff we're running memory or policy.
	if needsRaft(cfg.Functions) {
		if err := n.wireRaft(advertise); err != nil {
			n.closePartial()
			return nil, err
		}
		// Minimal PolicyService — Phase 4 replaces with the real engine.
		n.policySvc = policy.NewService(n.raft)
		lobslawv1.RegisterPolicyServiceServer(server, n.policySvc)
	}

	// Discovery client for seed-list exchange on Start.
	n.discCli = discovery.NewClient(local, registry, n.dialer(), log)

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

// Raft returns the underlying RaftNode for tests that need to call
// BootstrapCluster/AddVoter manually (multi-node test setup) or wait
// for leadership.
func (n *Node) Raft() *memory.RaftNode { return n.raft }

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
