package memory

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The gRPC face of transcript browsing.
//
// A thin wrapper on SessionService rather than a second reader: the
// agent's session_search tool and `lobslaw session search` already go
// through SearchTranscripts, so what an operator sees here is what the
// model would have found. Two search implementations would eventually
// disagree about what matched, and the operator would be the one
// debugging it.

// SessionRPC serves SessionService over gRPC.
type SessionRPC struct {
	lobslawv1.UnimplementedSessionServiceServer

	svc *SessionService
}

// NewSessionRPC wraps a session service for the wire.
func NewSessionRPC(svc *SessionService) *SessionRPC {
	return &SessionRPC{svc: svc}
}

func (r *SessionRPC) ListSessions(ctx context.Context, req *lobslawv1.ListSessionsRequest) (
	*lobslawv1.ListSessionsResponse, error) {
	if r.svc == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no session store")
	}
	records, err := r.svc.ListFiltered(ctx, req.GetChannel(), req.GetUserId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list sessions: %v", err)
	}
	return &lobslawv1.ListSessionsResponse{Sessions: records}, nil
}

func (r *SessionRPC) GetSession(ctx context.Context, req *lobslawv1.GetSessionRequest) (
	*lobslawv1.GetSessionResponse, error) {
	if r.svc == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no session store")
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}

	rec, err := r.svc.loadRecord(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get session: %v", err)
	}
	if rec == nil {
		// NOT_FOUND rather than an empty transcript. A conversation with
		// no messages and a conversation that does not exist are
		// different answers, and only one of them means the id was
		// wrong.
		return nil, status.Errorf(codes.NotFound,
			"no session with id %q — ListSessions returns the valid ids", req.GetId())
	}

	msgs, err := r.svc.LoadMessages(ctx, req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load transcript: %v", err)
	}
	return &lobslawv1.GetSessionResponse{Session: rec, Messages: msgs}, nil
}

func (r *SessionRPC) SearchSessions(ctx context.Context, req *lobslawv1.SearchSessionsRequest) (
	*lobslawv1.SearchSessionsResponse, error) {
	if r.svc == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node has no session store")
	}
	if req.GetText() == "" {
		// Enumeration is ListSessions. An empty search returning every
		// conversation would read as "they all mention it".
		return nil, status.Error(codes.InvalidArgument, "text is required; use ListSessions to enumerate")
	}

	hits, err := r.svc.SearchTranscripts(ctx, SessionSearchQuery{
		Text:               req.GetText(),
		Channel:            req.GetChannel(),
		UserID:             req.GetUserId(),
		Limit:              int(req.GetLimit()),
		SnippetsPerSession: int(req.GetSnippetsPerSession()),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "search transcripts: %v", err)
	}

	out := make([]*lobslawv1.SessionSearchHitProto, 0, len(hits))
	for _, h := range hits {
		snippets := make([]*lobslawv1.SessionSnippetProto, 0, len(h.Snippets))
		for _, sn := range h.Snippets {
			snippets = append(snippets, &lobslawv1.SessionSnippetProto{
				Seq: sn.Seq, Role: sn.Role, Text: sn.Text,
			})
		}
		out = append(out, &lobslawv1.SessionSearchHitProto{
			Session: h.Session,
			//nolint:gosec // a match count is bounded by the transcript, not by a caller
			Matches:  int32(h.Matches),
			Snippets: snippets,
		})
	}
	return &lobslawv1.SearchSessionsResponse{Hits: out}, nil
}
