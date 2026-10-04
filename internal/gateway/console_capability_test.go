package gateway

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/gateway/ui"
)

func TestUIBuildSupportIsSeparateFromRuntimeIntent(t *testing.T) {
	s := NewServer(RESTConfig{UIWebEnabled: true}, nil)
	caps := s.localCapabilities(context.Background())
	if caps.UIWeb.Supported != ui.Supported {
		t.Fatal("build support incorrect")
	}
	if !caps.UIWeb.Enabled || !caps.UIWeb.Configured {
		t.Fatal("runtime request was lost")
	}
	if caps.UIWeb.Available {
		t.Fatal("unmounted UI advertised available")
	}
}

func TestTeamReadinessChangesWithoutReplacingDependencies(t *testing.T) {
	ready := false
	s := NewServer(RESTConfig{Bots: &memBots{}, Groups: &memGroups{}, TeamsReady: func() bool { return ready }}, nil)
	caps := s.localCapabilities(context.Background())
	if !caps.ComputeTeams.Configured || caps.ComputeTeams.Available {
		t.Fatal(caps)
	}
	if _, err := s.consoleOperations().Bots(context.Background(), nil); err == nil {
		t.Fatal("dormant operations allowed")
	}
	ready = true
	if !s.localCapabilities(context.Background()).ComputeTeams.Available {
		t.Fatal("capabilities did not activate")
	}
}
