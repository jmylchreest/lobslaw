package trace

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Asking a SPECIFIC node what it recorded.
//
// Traces are per-node files, deliberately: R24 kept them out of raft so
// a trace never costs a replicated write. That decision stands. What
// was missing is a way to reach one from anywhere else — `lobslaw
// trace` read a directory on the local filesystem, so on an operator's
// laptop it either found nothing or, worse, found a stale copy and
// reported it as the cluster's.
//
// Every response carries the node id. A trace listing that does not
// say whose it is invites exactly the wrong conclusion.
//
// NO SPAN CARRIES CONTENT. Names, sizes, counts, timings and outcomes
// only. That is a property of Span, and this conversion is not the
// place it gets quietly relaxed.

// Service serves TraceService over gRPC.
type Service struct {
	lobslawv1.UnimplementedTraceServiceServer

	nodeID string
	dir    string
	// enabled distinguishes a node with tracing OFF from one that has
	// served no turns. Both have nothing to show and they are different
	// answers — only one of them is fixed by editing config.
	enabled bool
}

// NewService returns a trace reader for one node's directory.
func NewService(nodeID, dir string, enabled bool) *Service {
	return &Service{nodeID: nodeID, dir: dir, enabled: enabled}
}

func (s *Service) ListTurns(_ context.Context, req *lobslawv1.ListTurnsRequest) (
	*lobslawv1.ListTurnsResponse, error) {
	out := &lobslawv1.ListTurnsResponse{NodeId: s.nodeID, Enabled: s.enabled}
	if !s.enabled || s.dir == "" {
		// Not an error. A node with tracing off is a configuration, not
		// a failure, and the caller can tell which because Enabled says
		// so.
		return out, nil
	}
	ids, err := ListTurns(s.dir, int(req.GetLimit()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list turns: %v", err)
	}
	out.TurnIds = ids
	return out, nil
}

func (s *Service) ReadTurn(_ context.Context, req *lobslawv1.ReadTurnRequest) (
	*lobslawv1.ReadTurnResponse, error) {
	if req.GetTurnId() == "" {
		return nil, status.Error(codes.InvalidArgument, "turn_id is required")
	}
	out := &lobslawv1.ReadTurnResponse{NodeId: s.nodeID, Enabled: s.enabled}
	if !s.enabled || s.dir == "" {
		return out, nil
	}
	spans, err := ReadTurn(s.dir, req.GetTurnId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read turn: %v", err)
	}
	for _, sp := range spans {
		out.Spans = append(out.Spans, SpanToProto(sp))
	}
	return out, nil
}

// SpanToProto converts one span for the wire.
//
// Spelled out field by field rather than reflected or marshalled
// wholesale. A future field on Span that carried content would then
// have to be added HERE to escape, which is a decision somebody makes
// on purpose rather than one that happens to them.
func SpanToProto(s Span) *lobslawv1.TraceSpan {
	out := &lobslawv1.TraceSpan{
		TurnId:   s.TurnID,
		SpanId:   s.SpanID,
		ParentId: s.ParentID,
		Kind:     string(s.Kind),
		Name:     s.Name,
		Provider: s.Provider,
		//nolint:gosec // a nanosecond duration does not overflow int64 here
		DurationNs: int64(s.Duration),
		Outcome:    string(s.Outcome),
		//nolint:gosec // token counts are bounded by the provider's response
		PromptTokens: int32(s.Usage.PromptTokens),
		//nolint:gosec // token counts are bounded by the provider's response
		CompletionTokens: int32(s.Usage.CompletionTokens),
		//nolint:gosec // token counts are bounded by the provider's response
		CachedTokens: int32(s.Usage.CachedTokens),
		CostUsd:      s.CostUSD,
		//nolint:gosec // a result size is bounded by the tool's output cap
		ResultBytes: int32(s.ResultSize),
		Unit:        s.Unit,
		Quantity:    s.Quantity,
		BilledTo:    s.BilledTo,
		Error:       s.Error,
		//nolint:gosec // a failover position is single digits
		Attempt: int32(s.Attempt),
	}
	if !s.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(s.StartedAt)
	}
	return out
}

// SpanFromProto is the inverse, so the CLI renders a remote turn
// through the same code as a local one.
//
// Two renderers would drift, and the one that drifts is the cost
// total — which is the only reason anybody opens this command.
func SpanFromProto(p *lobslawv1.TraceSpan) Span {
	out := Span{
		TurnID:   p.GetTurnId(),
		SpanID:   p.GetSpanId(),
		ParentID: p.GetParentId(),
		Kind:     Kind(p.GetKind()),
		Name:     p.GetName(),
		Provider: p.GetProvider(),
		Duration: time.Duration(p.GetDurationNs()),
		Outcome:  Outcome(p.GetOutcome()),
		Usage: Usage{
			PromptTokens:     int(p.GetPromptTokens()),
			CompletionTokens: int(p.GetCompletionTokens()),
			CachedTokens:     int(p.GetCachedTokens()),
		},
		CostUSD:    p.GetCostUsd(),
		ResultSize: int(p.GetResultBytes()),
		Unit:       p.GetUnit(),
		Quantity:   p.GetQuantity(),
		BilledTo:   p.GetBilledTo(),
		Error:      p.GetError(),
		Attempt:    int(p.GetAttempt()),
	}
	if p.GetStartedAt() != nil {
		out.StartedAt = p.GetStartedAt().AsTime()
	}
	return out
}
