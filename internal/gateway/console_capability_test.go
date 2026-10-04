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
