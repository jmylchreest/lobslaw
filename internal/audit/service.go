package audit

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Service exposes the audit log over gRPC. Append routes through
// the AuditLog coordinator (which fans out to every configured sink
// under a shared hash chain); Query + VerifyChain scope to a single
// sink so a compromised Raft leader can't hide its tampering behind
// a clean-looking reply from its local sink.
type Service struct {
	lobslawv1.UnimplementedAuditServiceServer

	log *AuditLog
}

// NewService wires the gRPC surface to an AuditLog coordinator.
func NewService(log *AuditLog) (*Service, error) {
	if log == nil {
		return nil, errors.New("audit.Service: AuditLog required")
	}
	return &Service{log: log}, nil
}

func (s *Service) Append(ctx context.Context, req *lobslawv1.AppendRequest) (*lobslawv1.AppendResponse, error) {
	if req == nil || req.Entry == nil {
		return nil, status.Error(codes.InvalidArgument, "entry is required")
	}
	entry, err := s.log.AppendEntry(ctx, protoToTyped(req.Entry))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "append: %v", err)
	}
	return &lobslawv1.AppendResponse{Id: entry.ID}, nil
}

func (s *Service) Query(ctx context.Context, req *lobslawv1.QueryRequest) (*lobslawv1.QueryResponse, error) {
	if req == nil {
		req = &lobslawv1.QueryRequest{}
	}
	filter := types.AuditFilter{
		ActorScope: req.ActorScope,
		Action:     req.Action,
		Target:     req.Target,
		Limit:      int(req.Limit),
	}
	if req.Since != nil {
		filter.Since = req.Since.AsTime()
	}
	if req.Until != nil {
		filter.Until = req.Until.AsTime()
	}
	entries, err := s.log.Query(ctx, req.Sink, filter)
	if err != nil {
		return nil, toStatus("query", err)
	}
	out := make([]*lobslawv1.AuditEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, typedToProto(e))
	}
	return &lobslawv1.QueryResponse{Entries: out}, nil
}

// VerifyChain walks the named sink's chain ("" → every sink). The
// per-sink results are flattened: if any sink breaks, ok=false and
// first_break_id carries the first break encountered. Operators who
// need sink-level detail should call VerifyChain per sink.
func (s *Service) VerifyChain(ctx context.Context, req *lobslawv1.VerifyChainRequest) (*lobslawv1.VerifyChainResponse, error) {
	if req == nil {
		req = &lobslawv1.VerifyChainRequest{}
	}
	results, err := s.log.VerifyChain(ctx, req.Sink)
	if err != nil {
		return nil, toStatus("verify", err)
	}
	allOK := true
	var firstBreak string
	var totalChecked int64
	for _, r := range results {
		totalChecked += r.EntriesChecked
		if !r.OK {
			allOK = false
			if firstBreak == "" {
				firstBreak = r.FirstBreakID
			}
		}
	}
	return &lobslawv1.VerifyChainResponse{
		Ok:             allOK,
		FirstBreakId:   firstBreak,
		EntriesChecked: totalChecked,
	}, nil
}

func toStatus(op string, err error) error {
	if err != nil && strings.Contains(err.Error(), "unknown sink") {
		return status.Errorf(codes.InvalidArgument, "%s: %v", op, err)
	}
	return status.Errorf(codes.Internal, "%s: %v", op, err)
}
