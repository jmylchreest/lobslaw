package main

import "testing"

func TestParseClawhubRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantName  string
		wantVer   string
		shouldErr bool
	}{
		{"clawhub:gws-workspace@1.2.3", "gws-workspace", "1.2.3", false},
		{"clawhub:foo@1.0.0", "foo", "1.0.0", false},
		{"clawhub:foo@bar@2.0.0", "foo@bar", "2.0.0", false}, // last @ wins
		{"clawhub:foo", "", "", true},                        // missing version
		{"clawhub:@1.0.0", "", "", true},                     // missing name
		{"clawhub:foo@", "", "", true},                       // empty version
		{"github:foo@1.0.0", "", "", true},                   // wrong scheme
		{"clawhub:", "", "", true},                           // empty
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, ver, err := parseClawhubRef(tc.in)
			if tc.shouldErr {
				if err == nil {
					t.Errorf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tc.wantName || ver != tc.wantVer {
				t.Errorf("got (%q, %q), want (%q, %q)", name, ver, tc.wantName, tc.wantVer)
			}
		})
	}
}
