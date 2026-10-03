package policy

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Provenance has to survive proto -> types -> proto, because that is
// the path every rule takes on its way back out of SyncRules. When
// types.PolicyRule had no CreatedBy field the round trip silently
// erased it, and `policy approvals --created-by` answered "no rules"
// on a live node while the store held plenty.
//
// Asserted on the WIRE value a client would receive rather than on the
// intermediate struct: that is what the CLI filters and prints.
func TestProvenanceSurvivesTheProtoRoundTrip(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 3, 5, 14, 30, 0, 0, time.UTC)
	in := &lobslawv1.PolicyRule{
		Id:        "approval-p7",
		Subject:   "user:alice",
		Action:    "tool:exec",
		Resource:  "shell_command",
		Effect:    "allow",
		Priority:  1,
		Scope:     "owner",
		CreatedBy: ApprovalRulePrefix + "p7",
		CreatedAt: timestamppb.New(created),
		Conditions: []*lobslawv1.Condition{
			{Key: "time_of_day", Op: "between", Value: "09:00-17:00"},
		},
	}

	out := ruleToProto(protoToRule(in))

	if out.GetCreatedBy() != in.GetCreatedBy() {
		t.Errorf("created_by = %q, want %q — provenance was dropped", out.GetCreatedBy(), in.GetCreatedBy())
	}
	if out.GetCreatedAt() == nil {
		t.Fatal("created_at was dropped entirely")
	}
	if got := out.GetCreatedAt().AsTime(); !got.Equal(created) {
		t.Errorf("created_at = %s, want %s", got, created)
	}
	// The fields that already round-tripped must keep doing so.
	if out.GetScope() != in.GetScope() {
		t.Errorf("scope = %q, want %q", out.GetScope(), in.GetScope())
	}
	if len(out.GetConditions()) != len(in.GetConditions()) {
		t.Fatalf("conditions = %d, want %d", len(out.GetConditions()), len(in.GetConditions()))
	}
	if c := out.GetConditions()[0]; c.GetKey() != "time_of_day" || c.GetOp() != "between" || c.GetValue() != "09:00-17:00" {
		t.Errorf("condition = %+v, want the one that went in", c)
	}
}

// A rule nobody minted carries no timestamp, and must not acquire one.
// An operator-authored rule that came back stamped would read as
// approval-derived to anything filtering on provenance.
func TestAnUnstampedRuleStaysUnstamped(t *testing.T) {
	t.Parallel()
	out := ruleToProto(types.PolicyRule{
		ID:       "operator-wrote-this",
		Subject:  "*",
		Action:   "tool:exec",
		Resource: "read_file",
		Effect:   types.EffectAllow,
		Priority: 20,
	})

	if out.GetCreatedBy() != "" {
		t.Errorf("created_by = %q, want empty", out.GetCreatedBy())
	}
	if out.GetCreatedAt() != nil {
		t.Errorf("created_at = %v, want nil for a rule with no provenance", out.GetCreatedAt().AsTime())
	}
}
