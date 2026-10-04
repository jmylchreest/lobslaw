package node

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmylchreest/lobslaw/internal/policy"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const automaticUpgradePoll = 5 * time.Second
const automaticUpgradeStable = 30 * time.Second

// All service dependencies are immutable after wiring. Only the controller's
// readiness/status crosses goroutines; pending plans belong to its single loop.
type automaticUpgradeController struct {
	n       *Node
	ready   atomic.Bool
	blocker atomic.Pointer[string]
	pending string
	since   time.Time
	mu      sync.Mutex
	stopped bool
	cancel  context.CancelFunc
	done    chan struct{}
}

func newAutomaticUpgradeController(n *Node) *automaticUpgradeController {
	return &automaticUpgradeController{n: n}
}

func (c *automaticUpgradeController) start(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped || c.done != nil || c.n.cfg.RestoreMode {
		return
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.done = make(chan struct{})
	c.ready.Store(true)
	go func() {
		defer close(c.done)
		defer c.ready.Store(false)
		ticker := time.NewTicker(automaticUpgradePoll)
		defer ticker.Stop()
		for {
			c.tick(ctx, time.Now())
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (c *automaticUpgradeController) stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.stopped = true
	c.ready.Store(false)
	if c.cancel != nil {
		c.cancel()
	}
	done := c.done
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (c *automaticUpgradeController) report(reason string) {
	old := c.blocker.Load()
	if old != nil && *old == reason {
		return
	}
	c.blocker.Store(&reason)
	c.n.log.Info("cluster automatic upgrade", "status", reason)
}

func (c *automaticUpgradeController) tick(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || !c.ready.Load() || c.n.cfg.RestoreMode || !c.n.raft.IsLeader() {
		c.pending = ""
		c.report("waiting for ready leadership")
		return
	}
	plan, err := c.n.raft.PlanAutomaticUpgrade(ctx)
	if err != nil {
		c.pending = ""
		c.report(err.Error())
		return
	}
	if plan.Fence != c.pending {
		c.pending, c.since = plan.Fence, now
	}
	if now.Sub(c.since) < automaticUpgradeStable {
		c.report(fmt.Sprintf("stabilizing contract %d %s", plan.Step.Target, plan.Step.Action))
		return
	}
	s := plan.Step
	req := &pb.ChangeUpgradeRequest{Action: s.Action, TransitionId: s.ID, Target: s.Target, ExpectedEpoch: s.Epoch}
	decision := policy.Decision{Effect: types.EffectAllow, RuleID: "automatic-contract-1-to-2"}
	service := upgradeService{node: c.n}
	actor := "cluster:" + c.n.cfg.NodeID
	// Admission precedes mutation. This code-defined authority is deliberately
	// unavailable to external callers; it can only apply a reviewed, fenced plan.
	if err := service.recordUpgradeActor(ctx, actor, req, decision, "admitted", nil); err != nil {
		c.pending = ""
		c.report("automatic upgrade audit admission failed: " + err.Error())
		return
	}
	_, err = c.n.raft.ChangeAutomaticUpgrade(ctx, plan)
	if auditErr := service.recordUpgradeActor(ctx, actor, req, decision, "completed", err); auditErr != nil {
		c.n.log.Warn("automatic upgrade outcome audit failed", "transition", s.ID, "err", auditErr)
	}
	// Never replay on an uncertain response or failed outcome audit. The next
	// iteration reads durable state and builds a new plan before doing anything.
	c.pending = ""
	if err != nil {
		c.report(err.Error())
		return
	}
	c.report(fmt.Sprintf("contract %d %s committed", s.Target, s.Action))
}
