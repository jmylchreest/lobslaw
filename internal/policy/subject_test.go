package policy

import (
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestValidateSubject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		subject string
		wantErr bool
	}{
		{"empty matches everyone", "", false},
		{"star matches everyone", "*", false},
		{"user kind", "user:alice", false},
		{"role kind", "role:admin", false},
		{"scope kind", "scope:owner", false},
		{"unknown kind channel", "channel:telegram", true},
		{"unknown kind subject", "subject:google:1234567890", true},
		{"bare string, no kind", "bare", true},
		{"known kind, empty value", "user:", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateSubject(c.subject)
			if (err != nil) != c.wantErr {
				t.Errorf("ValidateSubject(%q) = %v, wantErr %v", c.subject, err, c.wantErr)
			}
		})
	}
}

// The kinds list ValidateSubject checks against must be exactly the
// kinds subjectMatches implements. A listed kind that subjectMatches
// does not honour would validate a rule the engine then refuses to
// apply, the same silent-deny gap this exists to close in the other
// direction.
func TestMatchableSubjectKindsBindToSubjectMatches(t *testing.T) {
	t.Parallel()
	claims := &types.Claims{UserID: "match-value", Roles: []string{"match-value"}, Scope: "match-value"}

	for _, kind := range matchableSubjectKinds {
		subject := kind + ":match-value"
		if err := ValidateSubject(subject); err != nil {
			t.Errorf("ValidateSubject(%q) = %v, want nil (kind is listed)", subject, err)
		}
		if !subjectMatches(subject, claims) {
			t.Errorf("subjectMatches(%q) = false, want true (a claim carrying this kind's value)", subject)
		}
	}

	const unlisted = "channel:match-value"
	if err := ValidateSubject(unlisted); err == nil {
		t.Errorf("ValidateSubject(%q) = nil, want an error (kind is not listed)", unlisted)
	}
	if subjectMatches(unlisted, claims) {
		t.Errorf("subjectMatches(%q) = true, want false (kind is not listed, so it must not match)", unlisted)
	}
}
