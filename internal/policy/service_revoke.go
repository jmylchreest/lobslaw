package policy

import (
	"context"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Revoking an "always" approval from a laptop.
//
// `lobslaw policy revoke-approvals` opened state.db directly, which
// means the node had to be stopped — and on an operator's laptop there
// is no state.db to open at all. The offline form stays for a cluster
// that will not start; this is the routine one.
//
// The refusal that makes the command safe — it cannot touch a rule an
// operator wrote by hand — lives HERE rather than in the CLI. A check
// in the client is one an attacker replaces, and the whole value of
// "revocable" is that the revoking is narrow.

// RevokeApprovalRules deletes rules minted by an "always" approval.
func (s *Service) RevokeApprovalRules(_ context.Context, req *lobslawv1.RevokeApprovalRulesRequest) (
	*lobslawv1.RevokeApprovalRulesResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request required")
	}
	if len(req.GetIds()) == 0 && !req.GetAll() {
		// Revoking everything must be asked for. An empty id list read
		// as "all" turns a mistyped command into a policy change.
		return nil, status.Error(codes.InvalidArgument,
			"name ids to revoke, or set all=true; an empty list is not everything")
	}
	if len(req.GetIds()) > 0 && req.GetAll() {
		// Both set is ambiguous, and the ambiguity resolves to the
		// larger blast radius. Refused rather than guessed.
		return nil, status.Error(codes.InvalidArgument,
			"ids and all are mutually exclusive")
	}

	rules, err := s.approvals()
	if err != nil {
		return nil, err
	}

	minted, err := rules.FromApprovals()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list approval rules: %v", err)
	}
	isMinted := make(map[string]bool, len(minted))
	for _, r := range minted {
		isMinted[r.GetId()] = true
	}

	targets, refused, notFound := s.partition(req, minted, isMinted)

	out := &lobslawv1.RevokeApprovalRulesResponse{Refused: refused, NotFound: notFound}
	for _, id := range targets {
		if req.GetDryRun() {
			out.Revoked = append(out.Revoked, id)
			continue
		}
		// Revoke re-checks provenance against the store rather than
		// trusting the listing above. The listing is a read taken
		// earlier, and a rule could have been replaced in between.
		if rerr := rules.Revoke(id); rerr != nil {
			return nil, status.Errorf(codes.Internal, "revoke %s: %v", id, rerr)
		}
		out.Revoked = append(out.Revoked, id)
	}
	return out, nil
}

// partition splits the request into what will be revoked, what is
// protected, and what does not exist.
//
// Protected and missing are kept apart because they are different
// mistakes: one is a rule somebody wrote deliberately, the other is a
// typo, and telling an operator "not revoked" without saying which
// leaves them to guess.
func (s *Service) partition(req *lobslawv1.RevokeApprovalRulesRequest,
	minted []*lobslawv1.PolicyRule, isMinted map[string]bool) (targets, refused, notFound []string) {
	if req.GetAll() {
		for _, r := range minted {
			targets = append(targets, r.GetId())
		}
		sort.Strings(targets)
		return targets, nil, nil
	}

	for _, id := range req.GetIds() {
		switch {
		case isMinted[id]:
			targets = append(targets, id)
		case s.ruleExists(id):
			refused = append(refused, id)
		default:
			notFound = append(notFound, id)
		}
	}
	return targets, refused, notFound
}

// ruleExists reports whether a rule with this id is in the store at
// all, which is what separates "protected" from "no such rule".
func (s *Service) ruleExists(id string) bool {
	r, err := s.GetRule(id)
	return err == nil && r != nil && strings.TrimSpace(r.GetId()) != ""
}

// approvals builds the ApprovalRules helper over this service's raft
// and store, so the definition of an approval-minted rule has ONE
// home: internal/policy. A second copy of that prefix check on the
// wire would be a second authority for the same fact.
func (s *Service) approvals() (*ApprovalRules, error) {
	if s.raft == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node does not host raft")
	}
	rules, err := NewApprovalRules(s.raft, s.raft.FSM().Store())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "approval rules: %v", err)
	}
	return rules, nil
}
