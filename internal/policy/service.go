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
)

// Service is a minimal PolicyService implementation — enough for
// Phase 2.6's exit-criteria write path (AddRule round-trip through
// Raft + bbolt). Phase 4 replaces this with the full evaluation
// engine (Evaluate, SyncRules, RequestConfirmation with risk-tier
// logic + inline confirmation dispatch).
type Service struct {
	lobslawv1.UnimplementedPolicyServiceServer

	raft *memory.RaftNode
}

// NewService returns a Service backed by raft — every AddRule goes
// through raft.Apply so the rule replicates to every voter.
func NewService(raft *memory.RaftNode) *Service {
	return &Service{raft: raft}
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

// GetRule reads a rule by id from the memory store. Not in the
// PolicyService proto — exposed as a helper for tests and the
// Phase 2.6 integration flow. Phase 4's Evaluate RPC will build on
// the same store reads.
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
