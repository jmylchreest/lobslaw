package node

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"

	"github.com/jmylchreest/lobslaw/internal/gateway/ui"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestUIWebWithoutComputeRequiresBackend(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionUIWeb},
	})
	if !ui.Supported {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if !errors.Is(err, ErrUIWebBackendRequired) {
		t.Fatalf("got %v, want ErrUIWebBackendRequired", err)
	}
}

func TestUIWebWithComputeDoesNotRequireBackend(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionUIWeb, types.FunctionCompute},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUIWebWithBackendPasses(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionUIWeb},
		UIWeb:     config.UIWebConfig{Backend: "compute-1:7443"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUIWebOffIgnoresBackend(t *testing.T) {
	t.Parallel()
	err := validateUIWebBackend(Config{
		Functions: []types.NodeFunction{types.FunctionMemory},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExplicitConsoleBackendOverridesLocalAgent(t *testing.T) {
	n := &Node{
		cfg:   Config{Creds: soulTestCreds(t, t.TempDir(), "console-front"), UIWeb: config.UIWebConfig{Backend: "127.0.0.1:9"}},
		agent: &compute.Agent{}, log: slog.Default(),
	}
	runner, err := n.resolveTurnRunner()
	if err != nil {
		t.Fatal(err)
	}
	if n.remoteTurnConn == nil {
		t.Fatal("explicit remote backend silently selected local compute")
	}
	defer func() { _ = n.remoteTurnConn.Close() }()
	if _, ok := runner.(*compute.RemoteRunner); !ok {
		t.Fatalf("runner is %T", runner)
	}
}
