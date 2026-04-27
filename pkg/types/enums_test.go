package types

import "testing"

func TestEffectIsValid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   Effect
		want bool
	}{
		{EffectAllow, true},
		{EffectDeny, true},
		{EffectRequireConfirmation, true},
		{Effect(""), false},
		{Effect("ALLOW"), false},
		{Effect("bananas"), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.in), func(t *testing.T) {
			t.Parallel()
			if got := tt.in.IsValid(); got != tt.want {
				t.Errorf("Effect(%q).IsValid() = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTrustTierAtLeast(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		tier  TrustTier
		floor TrustTier
		want  bool
	}{
		{"local meets local", TrustLocal, TrustLocal, true},
		{"local meets private", TrustLocal, TrustPrivate, true},
		{"local meets public", TrustLocal, TrustPublic, true},
		{"private meets local", TrustPrivate, TrustLocal, false},
		{"private meets private", TrustPrivate, TrustPrivate, true},
		{"private meets public", TrustPrivate, TrustPublic, true},
		{"public meets local", TrustPublic, TrustLocal, false},
		{"public meets private", TrustPublic, TrustPrivate, false},
		{"public meets public", TrustPublic, TrustPublic, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.tier.AtLeast(tt.floor); got != tt.want {
				t.Errorf("%q.AtLeast(%q) = %v, want %v", tt.tier, tt.floor, got, tt.want)
			}
		})
	}
}

func TestTrustTierIsValid(t *testing.T) {
	t.Parallel()
	cases := map[TrustTier]bool{
		TrustLocal:        true,
		TrustPrivate:      true,
		TrustPublic:       true,
		TrustTier(""):     false,
		TrustTier("weak"): false,
	}
	for in, want := range cases {
		t.Run(string(in), func(t *testing.T) {
			t.Parallel()
			if got := in.IsValid(); got != want {
				t.Errorf("TrustTier(%q).IsValid() = %v, want %v", in, got, want)
			}
		})
	}
}

func TestRiskTierIsValid(t *testing.T) {
	t.Parallel()
	cases := map[RiskTier]bool{
		RiskReversible:      true,
		RiskCommunicating:   true,
		RiskIrreversible:    true,
		RiskTier(""):        false,
		RiskTier("harmful"): false,
	}
	for in, want := range cases {
		t.Run(string(in), func(t *testing.T) {
			t.Parallel()
			if got := in.IsValid(); got != want {
				t.Errorf("RiskTier(%q).IsValid() = %v, want %v", in, got, want)
			}
		})
	}
}

func TestParseRetention(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantValid bool
	}{
		{"session", true},
		{"episodic", true},
		{"long-term", true},
		{"long_term", true},
		{"LONG-TERM", true},
		{"", true}, // empty parses to UNSPECIFIED, no error
		{"forever", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRetention(tc.in)
			if (err == nil) != tc.wantValid {
				t.Errorf("ParseRetention(%q): got err=%v, wantValid=%v", tc.in, err, tc.wantValid)
			}
		})
	}
}
