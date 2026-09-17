package gateway

import (
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// A team somebody claimed cannot be renamed or deleted by anybody else.
//
// Multi-user made this necessary the moment it landed. Before, one
// anonymous console session owned everything and ownership was a
// distinction without a difference; with two people signed in, Sam
// could rename and delete James's teams.
func TestOnlyTheOwnerMayChangeATeam(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		owner     string
		principal string
		want      bool
	}{
		{
			name:      "the owner may",
			owner:     "user:james",
			principal: "user:james",
			want:      true,
		},
		{
			name:      "somebody else may not",
			owner:     "user:james",
			principal: "user:sam",
			want:      false,
		},
		{
			// The seeded default team has no owner, as does every team
			// created before this field existed. Locking those would
			// shut people out of their own default on upgrade, which
			// is a worse failure than the one being prevented.
			name:      "an unowned team stays editable",
			owner:     "",
			principal: "user:sam",
			want:      true,
		},
		{
			name:      "an unauthenticated caller cannot take an owned team",
			owner:     "user:james",
			principal: "",
			want:      false,
		},
		{
			// Empty principal must not inherit the "unowned = editable"
			// rule — that rule is for signed-in operators on upgrade,
			// not for anonymous callers on a require_auth node.
			name:      "an unauthenticated caller cannot take an unowned team either",
			owner:     "",
			principal: "",
			want:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &lobslawv1.GroupRecord{Id: "core", Owner: tc.owner}
			if got := groupMayModify(rec, tc.principal); got != tc.want {
				t.Errorf("groupMayModify = %v, want %v", got, tc.want)
			}
		})
	}
}
