package node

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/pkg/types"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

type computerProjectAuthorizer struct{}

func (computerProjectAuthorizer) AuthorizeProject(context.Context, string, string) error { return nil }

func TestComputerWiringIsExplicit(t *testing.T) {
	t.Parallel()
	n := &Node{}
	n.wireComputer(computerProjectAuthorizer{})
	if n.computer != nil {
		t.Fatal("computer enabled without explicit config")
	}
	n.cfg.Computer = config.ComputerConfig{Enabled: true}
	n.wireComputer(nil)
	if n.computer != nil {
		t.Fatal("computer wired without ownership service")
	}
	n.wireComputer(computerProjectAuthorizer{})
	if n.computer == nil {
		t.Fatal("computer consumer adapter missing")
	}
	_ = n.computer.Close()
}

func TestBrowserToolRegistrationRequiresBothFeatureGates(t *testing.T) {
	t.Parallel()
	n := &Node{toolRegistry: tools.NewRegistry(), builtinsRegistry: tools.NewBuiltins()}
	n.cfg.Computer = config.ComputerConfig{Enabled: true}
	n.wireComputer(computerProjectAuthorizer{})
	defer func() { _ = n.computer.Close() }()
	resolve := func(context.Context) (string, string, error) { return "user:alice", "project", nil }
	if err := n.registerComputerTools(resolve); err != nil {
		t.Fatal(err)
	}
	if len(n.toolRegistry.List()) != 0 {
		t.Fatal("browser tools escaped compute-teams gate")
	}
	n.cfg.Functions = []types.NodeFunction{types.FunctionComputeTeams}
	if err := n.registerComputerTools(nil); err == nil {
		t.Fatal("browser tools registered without original-claim authorization")
	}
	if err := n.registerComputerTools(resolve); err != nil {
		t.Fatal(err)
	}
	if len(n.toolRegistry.List()) != len(tools.BrowserToolDefs()) {
		t.Fatal("browser tools absent from normal tool catalogue")
	}
}
