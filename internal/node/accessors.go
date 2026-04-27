package node

import (
	"github.com/jmylchreest/lobslaw/internal/audit"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/discovery"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/plan"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/internal/soul"
	"github.com/jmylchreest/lobslaw/internal/storage"
	"github.com/jmylchreest/lobslaw/pkg/auth"
)

// Trivial getters — kept here so node.go stays focused on assembly.
// Each returns a subsystem handle for callers (cmd/ binaries, tests,
// or other in-process consumers); nil-safe on nodes where the
// corresponding function isn't enabled.

func (n *Node) JWTValidator() *auth.Validator    { return n.jwtValidator }
func (n *Node) ListenAddr() string               { return n.listener.Addr().String() }
func (n *Node) Registry() *discovery.Registry    { return n.registry }
func (n *Node) Policy() *policy.Service          { return n.policySvc }
func (n *Node) Memory() *memory.Service          { return n.memorySvc }
func (n *Node) Raft() *memory.RaftNode           { return n.raft }
func (n *Node) Agent() *compute.Agent            { return n.agent }
func (n *Node) ToolRegistry() *compute.Registry  { return n.toolRegistry }
func (n *Node) Resolver() *compute.Resolver      { return n.resolver }
func (n *Node) Gateway() *gateway.Server         { return n.gatewaySrv }
func (n *Node) Plan() *plan.Service              { return n.planSvc }
func (n *Node) Scheduler() *scheduler.Scheduler  { return n.scheduler }
func (n *Node) Storage() *storage.Manager        { return n.storageMgr }
func (n *Node) StorageService() *storage.Service { return n.storageSvc }
func (n *Node) SkillRegistry() *skills.Registry  { return n.skillRegistry }
func (n *Node) Soul() *soul.Soul                 { return n.soul.Load() }
func (n *Node) Audit() *audit.AuditLog           { return n.auditLog }
func (n *Node) Discovery() *discovery.Service    { return n.discSvc }

// planServiceOrNil and skillDispatcherOrNil bridge nil-typed-pointers
// to interface-typed nils — Go's well-known gotcha where a nil *T
// stored in an interface compares as non-nil. The gateway / executor
// expect interface-typed nils for "no service wired."
func planServiceOrNil(svc *plan.Service) gateway.PlanService {
	if svc == nil {
		return nil
	}
	return svc
}

func skillDispatcherOrNil(a *skills.AgentAdapter) compute.SkillDispatcher {
	if a == nil {
		return nil
	}
	return a
}
