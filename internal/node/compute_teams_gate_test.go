package node

import (
	"context"
	"log/slog"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/tools"
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

func TestTeamServicesActivateWithoutRewiring(t *testing.T) {
	store := crossOwnerTestStore(t)
	n := &Node{store: store, log: slog.Default(), cfg: Config{Functions: []types.NodeFunction{types.FunctionMemory, types.FunctionCompute, types.FunctionComputeTeams}}, botSvc: memory.NewBotService(nil, store), toolRegistry: tools.NewRegistry(), builtinsRegistry: tools.NewBuiltins()}
	if err := n.wireComputeTeamsStage(); err != nil {
		t.Fatal(err)
	}
	bots := n.teamBotsOrNil()
	groups := n.teamGroupsOrNil()
	router := n.teamRouterOrNil()
	if bots == nil || groups == nil || router == nil {
		t.Fatal("team services were not wired for later activation")
	}
	if _, ok := n.toolRegistry.Get("ask_bot"); ok {
		t.Fatal("team tool exposed before activation")
	}
	if n.teamsActive() {
		t.Fatal("teams active before contract")
	}
	if got := router.BotForChannel(context.Background(), "web", "test", "alice"); got != "" {
		t.Fatal(got)
	}
	activateTeamFixture(t, store)
	if !n.teamsActive() {
		t.Fatal("teams did not activate")
	}
	if bots != n.teamBotsOrNil() || groups != n.teamGroupsOrNil() {
		t.Fatal("service replaced during activation")
	}
	if _, ok := n.toolRegistry.Get("ask_bot"); !ok {
		t.Fatal("team tool unavailable after activation")
	}
}
