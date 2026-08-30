package policy

import (
	"context"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Seeing and undoing "approved for the rest of this conversation".
//
// The permanent tier has had a listing and a revoke since it shipped,
// on the argument that "revocable" without a way to see and undo is a
// claim in a doc. The conversation tier had neither. The only way to
// drop one was /new, which also destroys the transcript — so somebody
// who regretted an approval had to choose between keeping it and
// losing the conversation it was given in.
//
// That is the wrong way round. A permanent grant is the one somebody
// stopped and thought about. A conversation grant is the one given
// mid-task to get on with something, and it is the one that had no
// surface at all.

// SetSessionGrants wires the conversation-grant store.
//
// Separate from NewService because the store needs the bbolt handle
// and a TTL from config, and the policy service is constructed before
// either is resolved. Nil leaves the two RPCs reporting Unimplemented
// rather than panicking — a node with no raft has no grants to list,
// and saying so is better than an empty list that reads as "none".
func (s *Service) SetSessionGrants(g *memory.SessionGrantStore) { s.grants = g }

// ListSessionGrants returns the standing conversation grants.
func (s *Service) ListSessionGrants(_ context.Context, req *lobslawv1.ListSessionGrantsRequest) (
	*lobslawv1.ListSessionGrantsResponse, error) {
	if s.grants == nil {
		return nil, status.Error(codes.Unimplemented,
			"session grants are not available on this node (no replicated store)")
	}
	sessionID := strings.TrimSpace(req.GetSessionId())

	var (
		grants []*lobslawv1.SessionGrant
		err    error
	)
	if sessionID != "" {
		grants, err = s.grants.ForSession(sessionID)
	} else {
		grants, err = s.grants.List()
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list session grants: %v", err)
	}
	// Conversation first, then operation. A listing an operator reads
	// to decide what to revoke should group the thing they are
	// deciding about; map order would reshuffle it every call.
	sort.SliceStable(grants, func(i, j int) bool {
		if grants[i].GetSessionId() != grants[j].GetSessionId() {
			return grants[i].GetSessionId() < grants[j].GetSessionId()
		}
		if grants[i].GetAction() != grants[j].GetAction() {
			return grants[i].GetAction() < grants[j].GetAction()
		}
		return grants[i].GetResource() < grants[j].GetResource()
	})
	return &lobslawv1.ListSessionGrantsResponse{
		Grants:     grants,
		TtlSeconds: int64(s.grants.TTL().Seconds()),
	}, nil
}

// RevokeSessionGrants drops standing grants, by id or by conversation.
//
// Dry run by default, matching RevokeApprovalRules: the caller opts
// into writing. Revoking is not destructive in the way a transcript
// delete is — the worst outcome is being asked again — but an operator
// reading a list and typing an id should see what they are about to do
// before it happens, and the two commands behaving differently would
// be its own trap.
func (s *Service) RevokeSessionGrants(ctx context.Context, req *lobslawv1.RevokeSessionGrantsRequest) (
	*lobslawv1.RevokeSessionGrantsResponse, error) {
	if s.grants == nil {
		return nil, status.Error(codes.Unimplemented,
			"session grants are not available on this node (no replicated store)")
	}
	ids := trimmed(req.GetIds())
	sessionID := strings.TrimSpace(req.GetSessionId())

	switch {
	case len(ids) == 0 && sessionID == "":
		// The same refusal RevokeApprovalRules makes: nobody should
		// revoke a set by leaving an argument off.
		return nil, status.Error(codes.InvalidArgument,
			"name ids to revoke, or a session_id to revoke a whole conversation's grants")
	case len(ids) > 0 && sessionID != "":
		return nil, status.Error(codes.InvalidArgument,
			"ids and session_id are alternatives; pass one")
	}

	if sessionID != "" {
		return s.revokeConversation(ctx, sessionID, req.GetDryRun())
	}
	return s.revokeIDs(ctx, ids, req.GetDryRun())
}

func (s *Service) revokeConversation(ctx context.Context, sessionID string, dryRun bool) (
	*lobslawv1.RevokeSessionGrantsResponse, error) {
	existing, err := s.grants.ForSession(sessionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read session grants: %v", err)
	}
	out := &lobslawv1.RevokeSessionGrantsResponse{}
	for _, g := range existing {
		out.Revoked = append(out.Revoked, g.GetId())
	}
	if dryRun || len(existing) == 0 {
		return out, nil
	}
	if _, err := s.grants.RevokeSession(ctx, sessionID); err != nil {
		return nil, status.Errorf(codes.Internal, "revoke session grants: %v", err)
	}
	return out, nil
}

func (s *Service) revokeIDs(ctx context.Context, ids []string, dryRun bool) (
	*lobslawv1.RevokeSessionGrantsResponse, error) {
	out := &lobslawv1.RevokeSessionGrantsResponse{}
	for _, id := range ids {
		if dryRun {
			// Reported against the live set, so a dry run that lists an
			// id as revocable is a promise the apply can keep.
			if s.grantExists(id) {
				out.Revoked = append(out.Revoked, id)
			} else {
				out.NotFound = append(out.NotFound, id)
			}
			continue
		}
		switch err := s.grants.Revoke(ctx, id); {
		case err == nil:
			out.Revoked = append(out.Revoked, id)
		case strings.Contains(err.Error(), memory.ErrGrantNotFound.Error()):
			// Separate from a failure: a typo and a broken store are
			// different mistakes with different fixes, and collapsing
			// them lets somebody believe they revoked something.
			out.NotFound = append(out.NotFound, id)
		default:
			return nil, status.Errorf(codes.Internal, "revoke %q: %v", id, err)
		}
	}
	return out, nil
}

func (s *Service) grantExists(id string) bool {
	all, err := s.grants.List()
	if err != nil {
		return false
	}
	for _, g := range all {
		if g.GetId() == id {
			return true
		}
	}
	return false
}

func trimmed(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
