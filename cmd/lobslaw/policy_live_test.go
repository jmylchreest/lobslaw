package main

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/policy"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// `policy approvals` and `revoke-approvals` opened state.db directly,
// which needs the node stopped — and on an operator's laptop there is
// no state.db to open. An empty list of grants then reads as "nothing
// outstanding" rather than as the wrong store.

// --- which way the command goes ----------------------------------------

// THE WIRING. Every subcommand goes LIVE without --offline.
func TestEveryPolicySubcommandIsLiveByDefault(t *testing.T) {
	for sub, form := range policyForms {
		if !routesTo(t, policyRoute(sub, false), form.live) {
			t.Errorf("%q without --offline does not reach its live form", sub)
		}
		if !routesTo(t, policyRoute(sub, true), form.offline) {
			t.Errorf("%q with --offline does not reach its offline form", sub)
		}
	}
}

func TestThePolicyFormsAreNotTheSame(t *testing.T) {
	for sub, form := range policyForms {
		if routesTo(t, form.live, form.offline) {
			t.Errorf("%q has the same function for live and offline", sub)
		}
	}
}

func TestAnUnknownPolicySubcommandHasNoRoute(t *testing.T) {
	if policyRoute("aprovals", false) != nil {
		t.Error("a mistyped subcommand resolved to something")
	}
}

// Without --offline, the command must try to REACH A NODE rather than
// look for a local state.db.
func TestPolicyApprovalsGoesLiveByDefault(t *testing.T) {
	noAmbientCluster(t)

	err := policyApprovalsLive(nil)
	if err == nil {
		t.Fatal("approvals with no connection details succeeded")
	}
	if strings.Contains(err.Error(), "state.db") {
		t.Errorf("the live path went looking for a local store: %v", err)
	}
}

// --- what counts as an approval-minted rule ----------------------------

func mintedRule(id string) *lobslawv1.PolicyRule {
	return &lobslawv1.PolicyRule{
		Id: id, Subject: "user:alice", Action: "tool:exec", Resource: "/usr/bin/rg",
		Effect: "allow", CreatedBy: policy.ApprovalRulePrefix + id,
		CreatedAt: timestamppb.Now(),
	}
}

// SyncRules returns EVERY rule. Listing without filtering would show an
// operator their own hand-written rules under the heading "grants an
// approval made", which is the opposite of the provenance this feature
// exists to give them.
func TestOnlyApprovalMintedRulesAreListed(t *testing.T) {
	all := []*lobslawv1.PolicyRule{
		mintedRule("b"),
		{Id: "operator-1", CreatedBy: "operator", Effect: "deny"},
		mintedRule("a"),
		{Id: "operator-2", CreatedBy: "", Effect: "allow"},
	}

	got := approvalRulesFrom(all)
	if len(got) != 2 {
		t.Fatalf("kept %d rules: %v", len(got), got)
	}
	// Sorted, so the output is stable between runs.
	if got[0].GetId() != "a" || got[1].GetId() != "b" {
		t.Errorf("order = %s, %s", got[0].GetId(), got[1].GetId())
	}
	for _, r := range got {
		if !strings.HasPrefix(r.GetCreatedBy(), policy.ApprovalRulePrefix) {
			t.Errorf("%s is not approval-minted but was listed", r.GetId())
		}
	}
}

// A rule with no provenance is not an approval. Treating an empty
// created_by as a match would list every rule written before
// provenance existed.
func TestARuleWithNoProvenanceIsNotAnApproval(t *testing.T) {
	got := approvalRulesFrom([]*lobslawv1.PolicyRule{{Id: "x"}})
	if len(got) != 0 {
		t.Errorf("a rule with no created_by was listed as an approval: %v", got)
	}
}

// --- saying where the answer came from ---------------------------------

// An empty list is indistinguishable from the wrong store unless the
// source is on the page. That is the failure R28 names.
func TestAnEmptyApprovalListNamesItsSource(t *testing.T) {
	var buf bytes.Buffer
	if err := renderApprovals(&buf, nil, "prod.example:9090", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "prod.example:9090") {
		t.Errorf("an empty list does not say where it looked:\n%s", buf.String())
	}
}

func TestTheListingShowsWhatTheGrantAllows(t *testing.T) {
	var buf bytes.Buffer
	if err := renderApprovals(&buf, []*lobslawv1.PolicyRule{mintedRule("p1")}, "prod:9090", false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"p1", "user:alice", "tool:exec", "/usr/bin/rg"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing is missing %q:\n%s", want, out)
		}
	}
}

// --- reporting a revocation --------------------------------------------

// A protected rule and a typo are different mistakes with different
// fixes. "Not revoked" without saying which leaves the operator to
// guess, and the wrong guess is "I must have already revoked it".
func TestTheReportSeparatesProtectedFromMissing(t *testing.T) {
	var buf bytes.Buffer
	renderRevocation(&buf, "prod:9090", []string{"approval:p1"},
		[]string{"operator-rule"}, []string{"ghost"}, true)
	out := buf.String()

	if !strings.Contains(out, "not approval-minted") || !strings.Contains(out, "operator-rule") {
		t.Errorf("a protected rule is not explained:\n%s", out)
	}
	if !strings.Contains(out, "no such rule") || !strings.Contains(out, "ghost") {
		t.Errorf("an unknown id is not reported:\n%s", out)
	}
}

// A dry run must not read as a completed revocation.
func TestADryRunSaysNothingWasWritten(t *testing.T) {
	var buf bytes.Buffer
	renderRevocation(&buf, "prod:9090", []string{"approval:p1"}, nil, nil, false)
	out := buf.String()
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("a dry run does not announce itself:\n%s", out)
	}
	if strings.Contains(out, "REVOKED") {
		t.Errorf("a dry run reports rules as revoked:\n%s", out)
	}
}

func TestAnAppliedRevocationSaysSo(t *testing.T) {
	var buf bytes.Buffer
	renderRevocation(&buf, "prod:9090", []string{"approval:p1"}, nil, nil, true)
	if !strings.Contains(buf.String(), "REVOKED") {
		t.Errorf("an applied revocation is not announced:\n%s", buf.String())
	}
}

// --- refusing before dialling ------------------------------------------

// Naming nothing is not "everything" — checked here as well as at the
// server, so the operator gets the refusal without a round trip.
func TestRevokeNeedsIdsOrAll(t *testing.T) {
	noAmbientCluster(t)

	err := policyRevokeLive(nil)
	if err == nil {
		t.Fatal("a revoke naming nothing was accepted")
	}
	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("error %q does not say how to revoke everything", err)
	}
}

func TestRevokeRefusesIdsAndAllTogether(t *testing.T) {
	noAmbientCluster(t)

	err := policyRevokeLive([]string{"--all", "approval:p1"})
	if err == nil {
		t.Fatal("--all with explicit ids was accepted")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not explain the conflict", err)
	}
}

// THE FLAG THAT MUST NEVER INVERT. DryRun is the inverse of --apply,
// and a dry run that writes is the flag doing the opposite of what it
// says — on a command whose whole job is deleting policy.
func TestApplyIsTheInverseOfDryRun(t *testing.T) {
	withApply, err := revokeRequest([]string{"approval:p1"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if withApply.GetDryRun() {
		t.Error("--apply still asked for a dry run; nothing would be revoked")
	}

	without, err := revokeRequest([]string{"approval:p1"}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !without.GetDryRun() {
		t.Error("no --apply still wrote; the default is meant to be a dry run")
	}
}

// The ids reach the request, or --apply deletes nothing and reports
// success.
func TestTheNamedIdsReachTheRequest(t *testing.T) {
	req, err := revokeRequest([]string{"approval:p1", "approval:p2"}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.GetIds()) != 2 || req.GetAll() {
		t.Errorf("request = %+v", req)
	}

	all, err := revokeRequest(nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if !all.GetAll() || len(all.GetIds()) != 0 {
		t.Errorf("--all request = %+v", all)
	}
}

// --- the usage ---------------------------------------------------------

func TestEveryPolicySubcommandIsInTheUsage(t *testing.T) {
	for sub := range policyForms {
		if !strings.Contains(policyUsage, sub) {
			t.Errorf("usage does not mention %q", sub)
		}
	}
	if !strings.Contains(policyUsage, "--offline") {
		t.Error("usage does not mention --offline, so nobody finds the forensic path")
	}
}
