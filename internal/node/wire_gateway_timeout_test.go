package node

import (
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// WriteTimeout bounds the whole request-to-response window, so one
// shorter than HardTimeout kills the socket before the agent's own cap
// can produce the forced-summary reply. That shipped: a hardcoded 60s
// write deadline against a 90s hard timeout, so every turn between the
// two returned "Empty reply from server" for work that had completed
// server-side and written its artifacts.
func TestRESTWriteTimeoutExceedsHardTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.GatewayConfig
		want time.Duration
	}{
		{
			name: "derived from an explicit hard timeout",
			cfg:  config.GatewayConfig{HardTimeout: 5 * time.Minute},
			want: 5*time.Minute + 30*time.Second,
		},
		{
			name: "derived from the default when hard timeout is unset",
			cfg:  config.GatewayConfig{},
			want: gateway.DefaultHardTimeout + 30*time.Second,
		},
		{
			name: "an explicit write timeout is honoured as given",
			cfg:  config.GatewayConfig{HardTimeout: time.Minute, WriteTimeout: 10 * time.Second},
			want: 10 * time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := restWriteTimeout(tc.cfg); got != tc.want {
				t.Errorf("restWriteTimeout = %v, want %v", got, tc.want)
			}
		})
	}
}

// The ordering itself is the invariant worth pinning: whatever the
// hard timeout is, the derived write deadline must outlive it.
func TestDerivedWriteTimeoutIsAlwaysLooser(t *testing.T) {
	for _, hard := range []time.Duration{0, time.Second, 90 * time.Second, time.Hour} {
		cfg := config.GatewayConfig{HardTimeout: hard}
		effective := hard
		if effective <= 0 {
			effective = gateway.DefaultHardTimeout
		}
		if got := restWriteTimeout(cfg); got <= effective {
			t.Errorf("hard=%v: write timeout %v does not outlive it", hard, got)
		}
	}
}
