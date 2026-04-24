package compute

import (
	"testing"
)

func TestNormaliseToCronNatural(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"every 5m", "*/5 * * * *", false},
		{"every 15m", "*/15 * * * *", false},
		{"every 1h", "0 */1 * * *", false},
		{"every 6h", "0 */6 * * *", false},
		{"hourly", "0 * * * *", false},
		{"daily 08:00", "0 8 * * *", false},
		{"every day 17:30", "30 17 * * *", false},
		{"0 9 * * 1", "0 9 * * 1", false}, // cron passes through
		{"every 30s", "", true},           // sub-minute rejected
		{"every 60m", "", true},           // too large
		{"every 24h", "", true},           // too large
		{"daily 25:00", "", true},         // bad time
		{"total nonsense", "", true},
	}
	for _, tc := range cases {
		got, err := normaliseToCron(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("normaliseToCron(%q) = %q; want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normaliseToCron(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normaliseToCron(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
