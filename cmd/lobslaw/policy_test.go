package main

import (
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// "Revocable" is the justification for letting a user tap Always at
// all. If the CLI cannot separate the grants they made from the rules
// an operator wrote, it is not a revoke command, it is a policy wipe.

func policyTestStore(t *testing.T, rules ...*lobslawv1.PolicyRule) *memory.Store {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, r := range rules {
		raw, err := proto.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(memory.BucketPolicyRules, r.Id, raw); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func TestApprovalMintedRulesSeparatesProvenance(t *testing.T) {
	t.Parallel()
	store := policyTestStore(t,
		&lobslawv1.PolicyRule{
			Id: "approval:p2", Subject: "user:alice", Action: "tool:exec",
			Resource: "send_email", Effect: "allow",
			CreatedBy: "approval:p2", CreatedAt: timestamppb.Now(),
		},
		&lobslawv1.PolicyRule{
			Id: "approval:p1", Subject: "user:alice", Action: "tool:exec",
			Resource: "write_file", Effect: "allow",
			CreatedBy: "approval:p1", CreatedAt: timestamppb.Now(),
		},
		&lobslawv1.PolicyRule{
			Id: "operator-allow-all", Subject: "*", Action: "*",
			Resource: "*", Effect: "allow",
		},
		&lobslawv1.PolicyRule{
			// Provenance from somewhere else entirely. Not ours to touch.
			Id: "seeded-stdlib", Subject: "*", Action: "tool:exec",
			Resource: "read_file", Effect: "allow", CreatedBy: "seed:stdlib",
		},
	)

	got, err := approvalMintedRules(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("found %d approval rules, want 2: %+v", len(got), got)
	}
	// Sorted by id, so output is stable between runs.
	if got[0].Id != "approval:p1" || got[1].Id != "approval:p2" {
		t.Errorf("unsorted or wrong rules: %s, %s", got[0].Id, got[1].Id)
	}
	for _, r := range got {
		if r.CreatedBy == "seed:stdlib" || r.Id == "operator-allow-all" {
			t.Errorf("a rule nobody approved was listed as an approval: %+v", r)
		}
	}
}

func TestApprovalRuleJSONCarriesProvenance(t *testing.T) {
	t.Parallel()
	when := timestamppb.Now()
	m := approvalRuleJSON(&lobslawv1.PolicyRule{
		Id: "approval:p1", Subject: "user:alice", Action: "tool:exec",
		Resource: "write_file", Effect: "allow",
		CreatedBy: "approval:p1", CreatedAt: when,
	})
	if m["created_by"] != "approval:p1" {
		t.Errorf("created_by = %v; without it the caller cannot tell an approval from an operator rule", m["created_by"])
	}
	if m["created_at"] == nil {
		t.Error("no created_at; an operator reviewing grants cannot tell when this happened")
	}
	if m["effect"] != "allow" {
		t.Errorf("effect = %v, want allow", m["effect"])
	}
}

// `policy rules` is the complete set: no provenance filter, unlike
// approvals. An operator-authored rule and one an approval minted must
// both come back.

func TestPolicyReadAllRulesIncludesEveryProvenance(t *testing.T) {
	t.Parallel()
	store := policyTestStore(t,
		&lobslawv1.PolicyRule{
			Id: "approval:p1", Subject: "user:alice", Action: "tool:exec",
			Resource: "write_file", Effect: "allow", CreatedBy: "approval:p1",
		},
		&lobslawv1.PolicyRule{
			Id: "operator-allow-all", Subject: "*", Action: "*",
			Resource: "*", Effect: "allow",
		},
		&lobslawv1.PolicyRule{
			Id: "seeded-stdlib", Subject: "*", Action: "tool:exec",
			Resource: "read_file", Effect: "allow", CreatedBy: "seed:stdlib",
		},
	)

	got, err := policyReadAllRules(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("found %d rules, want 3 (unfiltered): %+v", len(got), got)
	}
}

// --- filter --------------------------------------------------------------

func TestFilterPolicyRules(t *testing.T) {
	t.Parallel()
	rules := []*lobslawv1.PolicyRule{
		{Id: "a", Subject: "user:alice", CreatedBy: "lobslaw-builtin-tools"},
		{Id: "b", Subject: "user:bob", CreatedBy: "approval:p1"},
		{Id: "c", Subject: "user:alice", CreatedBy: "operator"},
	}

	tests := []struct {
		name      string
		subject   string
		createdBy string
		wantIDs   []string
	}{
		{name: "no filter keeps everything", wantIDs: []string{"a", "b", "c"}},
		{name: "subject is an exact match, not a prefix", subject: "user:alice", wantIDs: []string{"a", "c"}},
		{name: "subject matching nothing keeps nothing", subject: "user:carol", wantIDs: nil},
		{name: "created-by is a prefix match", createdBy: "lobslaw-builtin-", wantIDs: []string{"a"}},
		{name: "both filters narrow together", subject: "user:alice", createdBy: "operator", wantIDs: []string{"c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := filterPolicyRules(rules, tt.subject, tt.createdBy)
			gotIDs := make([]string, 0, len(got))
			for _, r := range got {
				gotIDs = append(gotIDs, r.GetId())
			}
			if !equalStrings(gotIDs, tt.wantIDs) {
				t.Errorf("ids = %v, want %v", gotIDs, tt.wantIDs)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- sort ------------------------------------------------------------------

// Priority descending, id-tiebroken, so the same rule set prints in
// the same order every time: an operator diffing two runs needs
// nothing else to have changed for the ORDER to tell them so.
func TestSortPolicyRulesByPriorityThenID(t *testing.T) {
	t.Parallel()
	rules := []*lobslawv1.PolicyRule{
		{Id: "b", Priority: 5},
		{Id: "a", Priority: 5},
		{Id: "z", Priority: 10},
		{Id: "m", Priority: 0},
	}
	sortPolicyRules(rules)
	got := make([]string, len(rules))
	for i, r := range rules {
		got[i] = r.GetId()
	}
	want := []string{"z", "a", "b", "m"}
	if !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// --- JSON shape --------------------------------------------------------

func TestPolicyRuleJSONShape(t *testing.T) {
	t.Parallel()
	m := policyRuleJSON(&lobslawv1.PolicyRule{
		Id: "operator-1", Subject: "role:admin", Action: "tool:exec",
		Resource: "*", Effect: "deny", Priority: 42, CreatedBy: "",
	})
	want := map[string]any{
		"id": "operator-1", "subject": "role:admin", "action": "tool:exec",
		"resource": "*", "effect": "deny", "priority": int32(42), "created_by": "",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("m[%q] = %v (%T), want %v (%T)", k, m[k], m[k], v, v)
		}
	}
	if len(m) != len(want) {
		t.Errorf("m has %d fields, want exactly %d (the shape must stay stable): %+v", len(m), len(want), m)
	}
}
