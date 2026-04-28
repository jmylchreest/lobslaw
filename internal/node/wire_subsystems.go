package node

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/clawhub"
	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/oauth"
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
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Each stage method below is registered in nodeWireStages (wire.go)
// with a matching gate predicate. They run in slice order, so a
// stage that consumes another's output (e.g. wireScheduler reads
// n.raft set by wireRaftStage) MUST appear later. Cross-stage state
// flows through Node fields — never returned from the stage func.

// wireRaftStage adapts the existing wireRaft helper to the stage
// signature. The advertise address comes from n.advertise, set by
// New's pre-stage assembly.
func (n *Node) wireRaftStage() error {
	return n.wireRaft(n.advertise)
}

// wirePolicyService registers the gRPC PolicyService backed by raft.
// PolicyService reads from the local replica and writes via raft.Apply.
func (n *Node) wirePolicyService() error {
	n.policySvc = policy.NewService(n.raft)
	lobslawv1.RegisterPolicyServiceServer(n.server, n.policySvc)
	return nil
}

// wireMemoryService registers the gRPC MemoryService.
func (n *Node) wireMemoryService() error {
	n.memorySvc = memory.NewService(n.raft, n.store, n.log)
	lobslawv1.RegisterMemoryServiceServer(n.server, n.memorySvc)
	return nil
}

// wireUserPrefs constructs the per-user preferences service. Reads
// are local; writes go through raft. Solo deployments seed an
// "owner" record from operator config; team deployments add records
// at runtime via a future user_bind builtin.
func (n *Node) wireUserPrefs() error {
	n.userPrefsSvc = memory.NewUserPrefsService(n.raft, n.store)
	return nil
}

// wireCredentials constructs the encrypted credentials service +
// the in-memory OAuth device-flow tracker. Both ride on the
// already-wired raft + store. The cluster MemoryKey doubles as the
// token-encryption key (see DESIGN.md "credentials encryption" —
// one master secret, not two).
func (n *Node) wireCredentials() error {
	cs, err := memory.NewCredentialService(n.raft, n.store, n.cfg.MemoryKey)
	if err != nil {
		return fmt.Errorf("credentials service: %w", err)
	}
	n.credentialSvc = cs
	n.oauthTracker = oauth.NewTracker(n.log)
	providers, err := n.resolveOAuthProviders()
	if err != nil {
		return fmt.Errorf("oauth providers: %w", err)
	}
	n.oauthProviders = providers
	return nil
}

// wireSoulRaft constructs the raft-backed soul tune service + the
// Adjuster. The FSM change hook fires the goroutine-deferred refresh
// (see fsm.go callback contract for why deferring matters).
func (n *Node) wireSoulRaft() error {
	loadedSoul := n.soul.Load()
	soulTuneSvc := memory.NewSoulTuneService(n.raft, n.store)
	n.soulTuneSvc = soulTuneSvc
	adj, err := soul.NewAdjuster(soul.AdjusterConfig{
		Soul:  loadedSoul,
		Store: newRaftSoulTuneStore(soulTuneSvc),
	})
	if err != nil {
		// Non-fatal: log + leave Adjuster nil. Downstream
		// soul builtins notice and skip registration.
		n.log.Warn("soul: adjuster construction failed; soul_* tools unavailable", "err", err)
		return nil
	}
	n.soulAdjuster = adj
	n.fsm.SetSoulTuneChangeCallback(func() {
		// See fsm.go callback contract: under f.mu, must not take
		// any user-side lock the mutator might hold across raft.Apply.
		// Goroutine indirection avoids the soul-mu vs fsm-mu deadlock.
		go func() {
			if rerr := adj.RefreshTune(context.Background()); rerr != nil {
				n.log.Warn("soul: refresh tune from FSM apply failed", "err", rerr)
			}
		}()
	})
	return nil
}

// wirePlanService registers the gRPC PlanService for commitment +
// task management. Reads hit the local store; writes propagate via
// raft.
func (n *Node) wirePlanService() error {
	n.planSvc = plan.NewService(n.raft, 0)
	lobslawv1.RegisterPlanServiceServer(n.server, n.planSvc)
	return nil
}

// wireScheduler builds the scheduler + registers the dream / session-
// prune / research handlers that depend on memorySvc + scheduler
// being present. Start is called from Node.Run, not here.
func (n *Node) wireScheduler() error {
	handlers := scheduler.NewHandlerRegistry()
	sched, err := scheduler.NewScheduler(scheduler.Config{
		NodeID: n.cfg.NodeID,
		Logger: n.log,
	}, n.raft, handlers)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	n.scheduler = sched
	n.registerDreamHandler()
	n.registerSessionPruneHandler()
	// registerResearchHandler is deliberately NOT called here. It
	// requires n.agent + n.memorySvc + n.toolRegistry, which the
	// compute stage wires later. wireCompute calls it after agent
	// construction (alongside registerAgentTurnHandlers).
	return nil
}

// wireStorageStage constructs the storage manager + service when
// FunctionStorage is enabled (gate composes raft + storage). The
// FSM storage-change hook drives Reconcile on every replicated edit.
func (n *Node) wireStorageStage() error {
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
		return fmt.Errorf("storage: %w", err)
	}
	n.storageMgr = mgr
	n.storageSvc = svc
	lobslawv1.RegisterStorageServiceServer(n.server, svc)
	n.fsm.SetStorageChangeCallback(func() {
		if rerr := svc.Reconcile(context.Background()); rerr != nil {
			n.log.Warn("storage: reconcile failed", "err", rerr)
		}
	})
	return nil
}

// wireSkills constructs the skill registry + invoker + adapter.
// Constructed regardless of whether storage is on this node — an
// empty registry means "no skills" not "skill subsystem broken."
// Registry.Watch is deferred to Node.Start so the storage mount
// can be registered first.
func (n *Node) wireSkills() error {
	n.skillRegistry = skills.NewRegistry(n.log)
	cfg := skills.InvokerConfig{
		Registry:            n.skillRegistry,
		Storage:             n.storageMgr,
		Mounts:              n.mountResolver,
		ProxyURL:            n.subprocessProxyURL,
		BinaryInstallPrefix: n.cfg.Security.BinaryInstallPrefix,
	}
	if n.credentialSvc != nil {
		cfg.Credentials = newCredentialIssuerAdapter(n.credentialSvc, func(name string) (oauth.ProviderConfig, bool) {
			p, ok := n.oauthProviders[name]
			return p, ok
		})
	}
	skillInvoker, err := skills.NewInvoker(cfg)
	if err != nil {
		return fmt.Errorf("skills invoker: %w", err)
	}
	adapter, err := skills.NewAgentAdapter(n.skillRegistry, skillInvoker)
	if err != nil {
		return fmt.Errorf("skills adapter: %w", err)
	}
	n.skillAdapter = adapter
	return nil
}

// wireClawhub constructs the clawhub catalog client + installer when
// the operator declared a base URL. No-op when ClawhubBaseURL is
// empty — operators with no clawhub access just don't configure it.
//
// Signing is wired but defaults to "off" — no publisher infrastructure
// exists yet. When clawhub.ai starts publishing signed bundles, an
// operator-trust-store config block + CLI will land alongside it.
func (n *Node) wireClawhub() error {
	base := strings.TrimSpace(n.cfg.Security.ClawhubBaseURL)
	if base == "" {
		return nil
	}
	if n.storageMgr == nil {
		return nil
	}
	c, err := clawhub.NewClient(base)
	if err != nil {
		return fmt.Errorf("clawhub client: %w", err)
	}
	inst, err := clawhub.NewInstaller(clawhub.InstallerConfig{
		Client:  c,
		Storage: n.storageMgr,
		Policy:  clawhub.SigningOff,
	})
	if err != nil {
		return fmt.Errorf("clawhub installer: %w", err)
	}
	n.clawhubInstaller = inst
	n.log.Info("clawhub: installer wired", "base", base)
	return nil
}

// wireAuditStage adapts the existing wireAudit helper to the stage
// signature.
func (n *Node) wireAuditStage() error {
	return n.wireAudit(context.Background())
}

// wireSoulFallback runs after wireAuditStage (which is "always") to
// catch the case where wireSoulRaft didn't fire because this node
// has no raft stack. A compute-only node still needs a working
// Adjuster — the in-memory store gives it one without replication.
func (n *Node) wireSoulFallback() error {
	if n.soulAdjuster != nil {
		return nil
	}
	adj, err := soul.NewAdjuster(soul.AdjusterConfig{
		Soul:  n.soul.Load(),
		Store: soul.NewMemoryTuneStore(),
	})
	if err != nil {
		n.log.Warn("soul: in-memory adjuster construction failed; soul_* tools unavailable", "err", err)
		return nil
	}
	n.soulAdjuster = adj
	return nil
}

// wireComputeStage adapts the existing wireCompute helper.
func (n *Node) wireComputeStage() error {
	return n.wireCompute()
}

// wireAuthStage builds the JWT validator when the config declares a
// validation method (HS256 secret or JWKS URL). Without one the
// validator stays nil and channels run anonymous-with-default-scope.
func (n *Node) wireAuthStage() error {
	authCfg := auth.Config{
		Issuer:     n.cfg.Auth.Issuer,
		AllowHS256: n.cfg.Auth.AllowHS256,
		JWKSURL:    n.cfg.Auth.JWKSURL,
	}
	if n.cfg.Auth.AllowHS256 {
		secret, err := n.resolveAPIKey(n.cfg.Auth.JWTSecretRef)
		if err != nil {
			return fmt.Errorf("auth HS256 secret: %w", err)
		}
		authCfg.HS256Secret = secret
	}
	v, err := auth.NewValidator(authCfg)
	if err != nil {
		return fmt.Errorf("auth validator: %w", err)
	}
	n.jwtValidator = v
	return nil
}

// wireGatewayStage adapts the existing wireGateway helper.
func (n *Node) wireGatewayStage() error {
	return n.wireGateway()
}

// wireDiscoveryStage registers the NodeService + builds the discovery
// client. Always runs — even nodes that don't host raft expose
// ListPeers / Reload via NodeService (AddMember returns Unimplemented
// when raftMembership is nil).
func (n *Node) wireDiscoveryStage() error {
	var raftMembership discovery.RaftMembership
	if n.raft != nil {
		raftMembership = n.raft
	}
	n.discSvc = discovery.NewService(n.registry, n.localInfo, n.log, n.reloadSections, raftMembership)
	lobslawv1.RegisterNodeServiceServer(n.server, n.discSvc)
	n.discCli = discovery.NewClient(n.localInfo, n.registry, n.dialer(), n.log)
	return nil
}

// wireBroadcastStage spins up UDP auto-discovery when the operator
// turned it on. Constructed here; Start dispatches the goroutine so
// cancellation lines up with the lifecycle of the rest of the node.
func (n *Node) wireBroadcastStage() error {
	bc, err := discovery.NewBroadcaster(discovery.BroadcastConfig{
		Address:    n.cfg.BroadcastAddress,
		ListenAddr: n.cfg.BroadcastListenAddr,
		Interval:   n.cfg.BroadcastInterval,
		Local:      n.localInfo,
		Registry:   n.registry,
		Logger:     n.log,
	})
	if err != nil {
		return fmt.Errorf("broadcast setup: %w", err)
	}
	n.broadcaster = bc
	return nil
}
