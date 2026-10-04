package node

import (
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestGateComputeTeamsIsOffForOrdinaryCompute(t *testing.T) {
	t.Parallel()
	compute, _ := types.NormalizeFunctions([]types.NodeFunction{types.FunctionCompute})
	if gateComputeTeams(Config{Functions: compute}) {
		t.Fatal("ordinary compute must not enable compute-teams")
	}
	teams, _ := types.NormalizeFunctions([]types.NodeFunction{types.FunctionComputeTeams})
	if !gateComputeTeams(Config{Functions: teams}) {
		t.Fatal("FunctionComputeTeams must open the teams gate")
	}
	if gateComputeTeams(Config{Functions: teams, RestoreMode: true}) {
		t.Fatal("restore mode must pause the teams drain")
	}
}

func TestAskBotIsNotRegisteredWithoutComputeTeams(t *testing.T) {
	t.Parallel()
	n := &Node{cfg: Config{
		Functions: []types.NodeFunction{types.FunctionCompute},
	}}
	if err := n.wireTeamTools(); err != nil {
		t.Fatal(err)
	}
	if n.toolRegistry != nil {
		if _, ok := n.toolRegistry.Get("ask_bot"); ok {
			t.Fatal("ask_bot must not be registered on ordinary compute")
		}
	}
}
