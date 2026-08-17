package policy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `lobslaw policy revoke-approvals` opened state.db directly, so the
// node had to be stopped — and on an operator's laptop there is no
// state.db to open at all. This is the live form.
//
// What makes it safe to expose on the wire is that the scope is the
// SERVER'S. The command's promise is that revoking your approvals
// cannot touch a rule you wrote by hand, and a check living in the CLI
// is one an attacker replaces.

func newRevokeService(t *testing.T) *Service {
	t.Helper()
	dataDir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dataDir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	_, inmem := raft.NewInmemTransport("revoke-node")
	node, err := memory.NewRaft(memory.RaftConfig{
		NodeID: "revoke-node", LocalAddr: "revoke-node",
		DataDir: dataDir, Bootstrap: true, Transport: inmem,
	}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = node.Shutdown()
		_ = store.Close()
	})
	if err := node.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	return NewService(node)
}

// mintApproval adds an approval-minted rule and returns its id.
func mintApproval(t *testing.T, s *Service, promptID string) string {
	t.Helper()
	rules, err := s.approvals()
	if err != nil {
		t.Fatal(err)
	}
	rule, err := rules.Mint(context.Background(), MintRequest{
		PromptID: promptID,
		Subject:  "user:alice",
		Action:   "tool:exec",
		Resource: "/usr/bin/rg",
	})
	if err != nil {
		t.Fatal(err)
	}
	return rule.GetId()
}

// operatorRule writes a rule by hand, the way an operator does — with
// no approval provenance.
func operatorRule(t *testing.T, s *Service, id string) string {
	t.Helper()
	_, err := s.AddRule(context.Background(), &lobslawv1.AddRuleRequest{
		Rule: &lobslawv1.PolicyRule{
			Id: id, Subject: "role:admin", Action: "tool:exec",
			Resource: "/bin/sh", Effect: "allow",
			CreatedBy: "operator", CreatedAt: timestamppb.Now(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func revoke(t *testing.T, s *Service, req *lobslawv1.RevokeApprovalRulesRequest) (
	*lobslawv1.RevokeApprovalRulesResponse, error) {
	t.Helper()
	return s.RevokeApprovalRules(context.Background(), req)
}

// THE CENTRAL PROPERTY. An operator-authored rule named explicitly by
// id is refused, not deleted. "Revoke my approvals" removing a rule
// somebody wrote deliberately would be a silent policy change nobody
// asked for.
func TestAnOperatorRuleIsRefusedNotRevoked(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	id := operatorRule(t, s, "operator-rule-1")

	res, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{Ids: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetRevoked()) != 0 {
		t.Errorf("revoked %v; an operator's rule was deleted", res.GetRevoked())
	}
	if len(res.GetRefused()) != 1 || res.GetRefused()[0] != id {
		t.Errorf("refused = %v, want [%s]", res.GetRefused(), id)
	}
	// And it is still there — the response could say the right thing
	// while the store said otherwise.
	if got, gerr := s.GetRule(id); gerr != nil || got.GetId() != id {
		t.Errorf("the operator's rule is gone: %v %v", got, gerr)
	}
}

// A typo and a protected rule are different mistakes with different
// fixes, so they must not report the same way.
func TestAnUnknownIdIsNotFoundRatherThanRefused(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)

	res, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{Ids: []string{"no-such-rule"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetNotFound()) != 1 {
		t.Errorf("not_found = %v", res.GetNotFound())
	}
	if len(res.GetRefused()) != 0 {
		t.Errorf("refused = %v; a missing rule was reported as protected", res.GetRefused())
	}
}

func TestAnApprovalMintedRuleIsRevoked(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	id := mintApproval(t, s, "prompt-1")

	res, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{Ids: []string{id}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetRevoked()) != 1 || res.GetRevoked()[0] != id {
		t.Fatalf("revoked = %v, want [%s]", res.GetRevoked(), id)
	}
	if got, gerr := s.GetRule(id); gerr == nil && got.GetId() != "" {
		t.Error("the rule is still in the store after being reported revoked")
	}
}

// all=true takes every minted rule and NOTHING ELSE. This is the case
// where a scope bug does the most damage.
func TestAllRevokesOnlyTheMintedRules(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	a := mintApproval(t, s, "prompt-a")
	b := mintApproval(t, s, "prompt-b")
	kept := operatorRule(t, s, "operator-rule-1")

	res, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetRevoked()) != 2 {
		t.Fatalf("revoked %v, want both minted rules", res.GetRevoked())
	}
	for _, id := range []string{a, b} {
		if got, gerr := s.GetRule(id); gerr == nil && got.GetId() != "" {
			t.Errorf("%s survived --all", id)
		}
	}
	if got, gerr := s.GetRule(kept); gerr != nil || got.GetId() != kept {
		t.Error("--all deleted a rule an operator wrote")
	}
}

// Revoking everything must be ASKED FOR. An empty id list read as
// "all" turns a mistyped command into a policy change.
func TestNamingNothingIsNotEverything(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	id := mintApproval(t, s, "prompt-1")

	_, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{})
	if err == nil {
		t.Fatal("an empty request was accepted")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", status.Code(err))
	}
	if got, gerr := s.GetRule(id); gerr != nil || got.GetId() != id {
		t.Error("the rule was revoked by a request that named nothing")
	}
}

// Both set is ambiguous, and the ambiguity resolves to the larger
// blast radius. Refused rather than guessed.
func TestIdsAndAllTogetherAreRefused(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	id := mintApproval(t, s, "prompt-1")

	if _, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{
		Ids: []string{id}, All: true,
	}); err == nil {
		t.Fatal("ids and all together were accepted")
	}
	if got, gerr := s.GetRule(id); gerr != nil || got.GetId() != id {
		t.Error("an ambiguous request still deleted something")
	}
}

// A dry run must WRITE NOTHING while still reporting what it would do,
// or --apply is a flag nobody trusts.
func TestADryRunWritesNothing(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	id := mintApproval(t, s, "prompt-1")

	res, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{
		Ids: []string{id}, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetRevoked()) != 1 {
		t.Errorf("a dry run reported %v; it should still say what it would do", res.GetRevoked())
	}
	if got, gerr := s.GetRule(id); gerr != nil || got.GetId() != id {
		t.Fatal("a dry run deleted the rule")
	}
}

// A dry run over --all must not delete either.
func TestADryRunOverAllWritesNothing(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	id := mintApproval(t, s, "prompt-1")

	if _, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{All: true, DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if got, gerr := s.GetRule(id); gerr != nil || got.GetId() != id {
		t.Error("a dry run over --all deleted the rule")
	}
}

// Mixed input: the minted one goes, the operator's stays, and both are
// reported. Silently dropping either half lets somebody believe they
// revoked something they did not.
func TestAMixedRequestReportsBothOutcomes(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	minted := mintApproval(t, s, "prompt-1")
	kept := operatorRule(t, s, "operator-rule-1")

	res, err := revoke(t, s, &lobslawv1.RevokeApprovalRulesRequest{
		Ids: []string{minted, kept, "ghost"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetRevoked()) != 1 || res.GetRevoked()[0] != minted {
		t.Errorf("revoked = %v", res.GetRevoked())
	}
	if len(res.GetRefused()) != 1 || res.GetRefused()[0] != kept {
		t.Errorf("refused = %v", res.GetRefused())
	}
	if len(res.GetNotFound()) != 1 || res.GetNotFound()[0] != "ghost" {
		t.Errorf("not_found = %v", res.GetNotFound())
	}
}

func TestANilRequestIsRefused(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)
	if _, err := s.RevokeApprovalRules(context.Background(), nil); err == nil {
		t.Error("a nil request was accepted")
	}
}
