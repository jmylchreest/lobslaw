package node

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const upgradeAuditTimeout = 5 * time.Second

func (s *upgradeService) recordUpgrade(ctx context.Context, req *pb.ChangeUpgradeRequest, decision policy.Decision, phase string, result error) error {
	if s.node.auditLog == nil {
		return errors.New("upgrade audit unavailable")
	}
	actor := "unverified"
	if cert := grpcinterceptors.VerifiedPeerCert(ctx); cert != nil {
		actor = "peer:" + cert.Subject.CommonName
		if mtls.IsOperatorCert(cert) {
			actor = "operator:" + cert.Subject.CommonName
		}
	}
	return s.recordUpgradeActor(ctx, actor, req, decision, phase, result)
}

func (s *upgradeService) recordUpgradeActor(ctx context.Context, actor string, req *pb.ChangeUpgradeRequest, decision policy.Decision, phase string, result error) error {
	if s.node.auditLog == nil {
		return errors.New("upgrade audit unavailable")
	}
	effect := decision.Effect
	if phase == "denied" {
		effect = types.EffectDeny
	}
	outcome := "success"
	if result != nil {
		outcome = result.Error()
	}
	digest := sha256.Sum256([]byte(outcome))
	// Finish attribution even when the RPC caller disconnects. Never replay the
	// mutation because an outcome audit sink failed after commit.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upgradeAuditTimeout)
	defer cancel()
	return s.node.auditLog.Append(auditCtx, types.AuditEntry{
		ActorScope: actor, Action: "cluster.upgrade.write", Target: "cluster:*", PolicyRule: decision.RuleID, Effect: effect,
		Argv:       []string{req.GetAction(), "id=" + req.GetTransitionId(), "target=" + strconv.FormatUint(uint64(req.GetTarget()), 10), "epoch=" + strconv.FormatUint(req.GetExpectedEpoch(), 10), "member=" + req.GetTargetNodeId(), "phase=" + phase},
		ResultHash: fmt.Sprintf("%x", digest),
	})
}

func (s *upgradeService) auditUpgrade(ctx context.Context, req *pb.ChangeUpgradeRequest, decision policy.Decision, phase string, result error) {
	if err := s.recordUpgrade(ctx, req, decision, phase, result); err != nil && s.node.log != nil {
		s.node.log.Warn("upgrade audit append failed", "action", req.GetAction(), "transition", req.GetTransitionId(), "phase", phase, "err", err)
	}
}
