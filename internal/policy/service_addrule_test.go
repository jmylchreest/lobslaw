package policy

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// AddRule is reachable directly over gRPC by any authenticated
// caller, not only the config-seeding and approval-minting paths that
// already validate a subject before they get here. Without this check
// a caller could still write a rule the engine will never match: it
// stores cleanly and reads back in a listing exactly like a rule that
// works.
func TestAddRuleRejectsAnUnmatchableSubject(t *testing.T) {
	t.Parallel()
	s := newRevokeService(t)

	cases := []struct {
		name    string
		subject string
		wantErr bool
	}{
		{"unknown kind channel", "channel:telegram", true},
		{"unknown kind subject", "subject:google:1234567890", true},
		{"bare string, no kind", "bare", true},
		{"known kind, empty value", "user:", true},
		{"user kind", "user:alice", false},
		{"role kind", "role:admin", false},
		{"scope kind", "scope:owner", false},
		{"star matches everyone", "*", false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := s.AddRule(context.Background(), &lobslawv1.AddRuleRequest{
				Rule: &lobslawv1.PolicyRule{
					Id:       "test-rule-" + c.name,
					Subject:  c.subject,
					Action:   "tool:exec",
					Resource: "write_file",
					Effect:   "allow",
					Priority: int32(i),
				},
			})
			if c.wantErr {
				if err == nil {
					t.Fatalf("AddRule(subject=%q) should have been refused", c.subject)
				}
				if status.Code(err) != codes.InvalidArgument {
					t.Errorf("AddRule(subject=%q) code = %v, want InvalidArgument", c.subject, status.Code(err))
				}
				return
			}
			if err != nil {
				t.Fatalf("AddRule(subject=%q) should have stored: %v", c.subject, err)
			}
		})
	}
}
