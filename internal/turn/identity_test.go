package turn

import (
	"testing"
)

func TestTurnIdentityAttributedTo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   Identity
		want string
	}{
		{"scope and user", Identity{Scope: "admin", UserID: "alice"}, "admin:alice"},
		{"user only", Identity{UserID: "alice"}, "alice"},
		// Not ":alice" — a bare separator implies a scope that was
		// never established.
		{"scope only", Identity{Scope: "admin"}, ""},
		{"neither", Identity{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.AttributedTo(); got != tc.want {
				t.Errorf("AttributedTo() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTurnIdentitySessionKey(t *testing.T) {
	t.Parallel()
	id := Identity{Channel: "telegram", ChannelID: "42"}
	if got := id.SessionKey(); got.Channel != "telegram" || got.ChannelID != "42" {
		t.Errorf("SessionKey() = %+v", got)
	}
	if got := (Identity{}).SessionKey(); got != (SessionKey{}) {
		t.Errorf("channelless identity produced %+v, want zero", got)
	}
}

// The guard test is only worth having if it actually fires, so this
// asserts the detector against a synthetic violation rather than
// trusting that a clean run means it works.
