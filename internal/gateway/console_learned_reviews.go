package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func learnedReviewProto(r LearnedReview) *pb.ConsoleLearnedReview {
	out := &pb.ConsoleLearnedReview{Id: r.ID, Author: r.Author, Name: r.Name, Description: r.Description, Body: r.Body, Files: r.Files, Revision: r.Revision, Digest: r.Digest, TurnId: r.TurnID, Active: r.Active}
	if p := r.Pending; p != nil {
		out.Pending = &pb.ConsoleLearnedChange{Description: p.Description, Body: p.Body, Files: p.Files, Rationale: p.Rationale, TurnId: p.TurnID}
	}
	return out
}

func learnedReviewError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrLearnedReviewForbidden):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, ErrLearnedReviewNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrLearnedReviewConflict):
		return status.Error(codes.Aborted, err.Error())
	default:
		return status.Error(codes.Unavailable, "learned review failed: "+err.Error())
	}
}

// Both transports call the same policy-authorised human service, rather than
// approving task grants or exposing agent authoring/activation primitives.
func (s *Server) queryConsoleLearned(ctx context.Context, in *pb.QueryConsoleRequest) (*pb.QueryConsoleResponse, error) {
	if s.cfg.Learned == nil {
		return nil, status.Error(codes.Unavailable, "learned review unavailable on this backend")
	}
	claims := claimsFromProto(in.Identity.Claims)
	if in.GetLearnedReviews() != nil {
		rows, err := s.cfg.Learned.List(ctx, claims)
		if err != nil {
			return nil, learnedReviewError(err)
		}
		out := &pb.ConsoleLearnedReviews{}
		for _, row := range rows {
			out.Reviews = append(out.Reviews, learnedReviewProto(row))
		}
		return &pb.QueryConsoleResponse{Result: &pb.QueryConsoleResponse_LearnedReviews{LearnedReviews: out}}, nil
	}
	row, err := s.cfg.Learned.Get(ctx, claims, in.GetLearnedReview().GetId())
	if err != nil {
		return nil, learnedReviewError(err)
	}
	return &pb.QueryConsoleResponse{Result: &pb.QueryConsoleResponse_LearnedReview{LearnedReview: learnedReviewProto(row)}}, nil
}

func (s *Server) decideConsoleLearned(ctx context.Context, in *pb.MutateConsoleRequest) (*pb.MutateConsoleResponse, error) {
	if s.cfg.Learned == nil {
		return nil, status.Error(codes.Unavailable, "learned review unavailable on this backend")
	}
	q := in.GetDecideLearnedReview()
	if q.GetId() == "" || q.GetRevision() == 0 || q.GetDigest() == "" {
		return nil, status.Error(codes.InvalidArgument, "inspected proposal id, revision and digest required")
	}
	message, err := s.cfg.Learned.Decide(ctx, claimsFromProto(in.Identity.Claims), q.Id, q.Revision, q.Digest, q.Approve)
	if err != nil {
		return nil, learnedReviewError(err)
	}
	return &pb.MutateConsoleResponse{Result: &pb.MutateConsoleResponse_LearnedDecision{LearnedDecision: &pb.ConsoleLearnedDecisionResult{Message: message}}}, nil
}

const learnedDecisionBodyLimit int64 = 4096

func (s *Server) handleLearnedReviews(w http.ResponseWriter, r *http.Request) {
	authn, err := s.authenticateRequest(r)
	if err != nil || authn.Claims == nil || authn.Claims.UserID == "" || authn.Claims.UserID == "anon" {
		s.jsonErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	identity := consoleIdentity(authn.Claims)
	path := strings.TrimPrefix(r.URL.Path, "/v1/learned-reviews")
	var result proto.Message
	switch {
	case r.Method == http.MethodGet:
		q := &pb.QueryConsoleRequest{Identity: identity}
		if path == "" {
			q.Query = &pb.QueryConsoleRequest_LearnedReviews{LearnedReviews: &pb.ConsoleEmpty{}}
		} else {
			id := strings.TrimPrefix(path, "/")
			if id == "" || strings.Contains(id, "/") {
				s.jsonErr(w, http.StatusNotFound, "unknown review route")
				return
			}
			q.Query = &pb.QueryConsoleRequest_LearnedReview{LearnedReview: &pb.ConsoleTarget{Id: id}}
		}
		var out *pb.QueryConsoleResponse
		if s.cfg.RemoteConsole != nil {
			out, err = s.cfg.RemoteConsole.QueryConsole(r.Context(), q)
		} else {
			out, err = s.queryConsoleLearned(r.Context(), q)
		}
		if path == "" {
			result = out.GetLearnedReviews()
		} else {
			result = out.GetLearnedReview()
		}
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/decide"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/decide")
		if id == "" || strings.Contains(id, "/") {
			s.jsonErr(w, http.StatusNotFound, "unknown review route")
			return
		}
		decision, decodeErr := decodeLearnedDecision(w, r, id)
		if decodeErr != nil {
			consoleHTTPError(s, w, decodeErr)
			return
		}
		q := &pb.MutateConsoleRequest{Identity: identity, Operation: &pb.MutateConsoleRequest_DecideLearnedReview{DecideLearnedReview: decision}}
		var out *pb.MutateConsoleResponse
		if s.cfg.RemoteConsole != nil {
			out, err = s.cfg.RemoteConsole.MutateConsole(r.Context(), q)
		} else {
			out, err = s.decideConsoleLearned(r.Context(), q)
		}
		result = out.GetLearnedDecision()
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "GET a proposal or POST its decision")
		return
	}
	if err != nil {
		consoleHTTPError(s, w, err)
		return
	}
	if result == nil || !result.ProtoReflect().IsValid() {
		s.jsonErr(w, http.StatusBadGateway, "missing learned review result")
		return
	}
	// Like task approvals, protobuf JSON keeps revisions as exact decimal
	// strings and uses generated JSON names. Do not round-trip through float64.
	body, err := protojson.Marshal(result)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, "encode learned review")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func decodeLearnedDecision(w http.ResponseWriter, r *http.Request, id string) (*pb.ConsoleLearnedDecision, error) {
	var body struct {
		Revision json.Number `json:"revision"`
		Digest   string      `json:"digest"`
		Approve  *bool       `json:"approve"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, learnedDecisionBodyLimit))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, status.Error(codes.InvalidArgument, "one decision object required")
	}
	revision, err := strconv.ParseUint(string(body.Revision), 10, 64)
	if err != nil || revision == 0 || body.Digest == "" || body.Approve == nil {
		return nil, status.Error(codes.InvalidArgument, "revision, digest and explicit approve required")
	}
	return &pb.ConsoleLearnedDecision{Id: id, Revision: revision, Digest: body.Digest, Approve: *body.Approve}, nil
}
