package policy

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Service is the gRPC-facing PolicyService. AddRule writes through
// Raft; Evaluate + SyncRules read through the local engine which
// hits the FSM-backed bbolt store. RequestConfirmation stays
// Unimplemented until Phase 6 wires Channel.Prompt dispatch.
type Service struct {
	lobslawv1.UnimplementedPolicyServiceServer

	raft   *memory.RaftNode
	engine *Engine
}

// NewService returns a Service backed by raft — every AddRule goes
// through raft.Apply so the rule replicates to every voter. An Engine
// is constructed over the same store so Evaluate sees every Applied
// rule immediately.
func NewService(raft *memory.RaftNode) *Service {
	engine := NewEngine(raft.FSM().Store(), nil)
	return &Service{raft: raft, engine: engine}
}

// Engine exposes the policy engine so Phase 4.3 (tool executor) can
// call Evaluate directly without a gRPC round-trip for local checks.
func (s *Service) Engine() *Engine { return s.engine }

// Evaluate runs the rule set against the request and returns the
// effect. Reads hit the local store — no Raft round-trip needed for
// a read-only query.
func (s *Service) Evaluate(ctx context.Context, req *lobslawv1.EvaluateRequest) (*lobslawv1.EvaluateResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	claims := claimsFromProto(req.Claims)
	dec, err := s.engine.Evaluate(ctx, claims, req.Action, req.Resource)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "evaluate: %v", err)
	}
	return &lobslawv1.EvaluateResponse{
		Effect: string(dec.Effect),
		RuleId: dec.RuleID,
		Reason: dec.Reason,
	}, nil
}

// SyncRules returns the complete set of rules known to this node.
// Clients that want eventual consistency can call this periodically
// and reconcile against their local copy. Rule order is descending
// priority, id-tiebroken — matches the evaluation order.
func (s *Service) SyncRules(_ context.Context, _ *lobslawv1.SyncRulesRequest) (*lobslawv1.SyncRulesResponse, error) {
	rules, err := s.engine.loadRules()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load rules: %v", err)
	}
	out := make([]*lobslawv1.PolicyRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleToProto(r))
	}
	return &lobslawv1.SyncRulesResponse{Rules: out}, nil
}

// AddRule replicates a PolicyRule via Raft. The returned Id matches
// what was written (caller may pre-set req.Rule.Id or leave empty for
// auto-generation).
func (s *Service) AddRule(ctx context.Context, req *lobslawv1.AddRuleRequest) (*lobslawv1.AddRuleResponse, error) {
	if req == nil || req.Rule == nil {
		return nil, status.Error(codes.InvalidArgument, "rule is required")
	}
	if req.Rule.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "rule.id is required (auto-generation lands with Phase 4)")
	}

	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: req.Rule.Id,
		Payload: &lobslawv1.LogEntry_PolicyRule{
			PolicyRule: req.Rule,
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "marshal log entry: %v", err)
	}

	res, err := s.raft.Apply(data, 5*time.Second)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "raft apply: %v", err)
	}
	if fsmErr, ok := res.(error); ok && fsmErr != nil {
		return nil, status.Errorf(codes.Internal, "fsm apply: %v", fsmErr)
	}
	return &lobslawv1.AddRuleResponse{Id: req.Rule.Id}, nil
}

// GetRule reads a rule by id from the memory store. Helper for
// tests and the Phase 2.6 integration flow.
func (s *Service) GetRule(id string) (*lobslawv1.PolicyRule, error) {
	raw, err := s.raft.FSM().Store().Get(memory.BucketPolicyRules, id)
	if err != nil {
		return nil, fmt.Errorf("get rule %q: %w", id, err)
	}
	var rule lobslawv1.PolicyRule
	if err := proto.Unmarshal(raw, &rule); err != nil {
		return nil, fmt.Errorf("unmarshal rule: %w", err)
	}
	return &rule, nil
}

// claimsFromProto converts the wire Claims into the typed internal
// form. Timestamps pass through via protobuf's AsTime().
func claimsFromProto(p *lobslawv1.Claims) *types.Claims {
	if p == nil {
		return nil
	}
	c := &types.Claims{
		UserID:   p.UserId,
		Issuer:   p.Issuer,
		Audience: p.Audience,
		Scope:    p.Scope,
	}
	if len(p.Roles) > 0 {
		c.Roles = append([]string(nil), p.Roles...)
	}
	if p.ExpiresAt != nil {
		c.ExpiresAt = p.ExpiresAt.AsTime()
	}
	if p.IssuedAt != nil {
		c.IssuedAt = p.IssuedAt.AsTime()
	}
	return c
}

// ruleToProto is the inverse of the engine's protoToRule. Needed for
// SyncRules to return the stored rule set over the wire.
func ruleToProto(r types.PolicyRule) *lobslawv1.PolicyRule {
	conds := make([]*lobslawv1.Condition, 0, len(r.Conditions))
	for _, c := range r.Conditions {
		conds = append(conds, &lobslawv1.Condition{Key: c.Key, Op: c.Op, Value: c.Value})
	}
	return &lobslawv1.PolicyRule{
		Id:         r.ID,
		Subject:    r.Subject,
		Action:     r.Action,
		Resource:   r.Resource,
		Effect:     string(r.Effect),
		Conditions: conds,
		Priority:   int32(r.Priority),
		Scope:      r.Scope,
	}
}
