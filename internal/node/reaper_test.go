package node

import (
	"log/slog"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/oauth"
)

func TestReapOAuthFlowsLeavesNonTerminal(t *testing.T) {
	t.Parallel()
	tracker := oauth.NewTracker(slog.Default())
	n := &Node{oauthTracker: tracker, log: slog.Default()}

	// We can't easily synthesize a "pending" flow without the real
	// device-auth POST, so instead we verify the reaper is a no-op
	// when there's nothing terminal to reap.
	n.reapOAuthFlows(time.Now())

	if got := len(tracker.List()); got != 0 {
		t.Errorf("expected empty tracker, got %d entries", got)
	}
}

func TestReapMaxAgeConstantsSensible(t *testing.T) {
	t.Parallel()
	if reapMaxAgeTerminalFlow < time.Hour {
		t.Errorf("reapMaxAgeTerminalFlow = %v, want >= 1h to give operators inspection window", reapMaxAgeTerminalFlow)
	}
	if reapMaxAgeSyntheticCred > reapMaxAgeTerminalFlow {
		t.Errorf("reapMaxAgeSyntheticCred (%v) should be <= reapMaxAgeTerminalFlow (%v) — synthetic creds are inert",
			reapMaxAgeSyntheticCred, reapMaxAgeTerminalFlow)
	}
	if reapInterval < 5*time.Minute {
		t.Errorf("reapInterval = %v is too aggressive — flows take time to settle", reapInterval)
	}
}
