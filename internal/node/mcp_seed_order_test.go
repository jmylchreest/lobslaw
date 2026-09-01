package node

import (
	"os"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/tools"
)

// The MCP management builtins must be seeded like every other
// builtin, which means they have to be REGISTERED before the seed
// pass walks the registry.
//
// They were not: registerMCPToolsWithCompute ran sixty-five lines
// after seedDefaultPolicyRules, so mcp_list, mcp_add and mcp_remove
// got no tool:exec rule and every call landed in default-deny — a
// builtin that existed, listed, and refused, with debug_mcp working
// beside it because that one registers in the compute stage.
//
// Asserted on the source rather than by booting a node: the bug is
// an ORDERING, and a test that starts a node would pass for the
// wrong reason the moment the two moved into different functions.
func TestMCPManagementToolsRegisterBeforeSeeding(t *testing.T) {
	t.Parallel()
	src := readNodeSource(t)

	register := strings.Index(src, "n.registerMCPToolsWithCompute()")
	seed := strings.Index(src, "n.seedDefaultPolicyRules(ctx)")
	if register < 0 || seed < 0 {
		t.Fatalf("could not locate both call sites (register=%d seed=%d)", register, seed)
	}
	if register > seed {
		t.Error("MCP tools register after the policy seed pass; they will have no tool:exec rule " +
			"and every call will be denied by default")
	}
}

// The management builtins must exist on a node with no servers
// configured. They lived inside `if len(cfg.MCP.Servers) > 0`, which
// meant mcp_add — the tool for adding a server — was absent on a node
// with none.
func TestMCPManagementToolsDoNotRequireAConfiguredServer(t *testing.T) {
	t.Parallel()
	src := readNodeSource(t)

	branch := strings.Index(src, "if len(n.cfg.MCP.Servers) > 0 {")
	register := strings.Index(src, "n.registerMCPToolsWithCompute()")
	if branch < 0 || register < 0 {
		t.Fatal("could not locate the wireup block")
	}
	// The registration must sit outside the branch: after its closing
	// brace, not between the brace and the branch head.
	closing := strings.Index(src[branch:], "\n\t}\n")
	if closing < 0 {
		t.Fatal("could not find the end of the configured-servers branch")
	}
	if register < branch+closing {
		t.Error("registerMCPToolsWithCompute is inside `if len(cfg.MCP.Servers) > 0`; " +
			"mcp_add would not exist on a node with no servers, which is the node that needs it")
	}
}

// And the tools themselves must not be in the unseeded set — being
// registered in time only matters if the seeder is willing to grant
// them.
func TestMCPManagementToolsAreSeedable(t *testing.T) {
	t.Parallel()
	src := readNodeSource(t)
	seeds := readSeedSource(t)

	for _, td := range tools.MCPManagementToolDefs() {
		if strings.Contains(seeds, `"`+td.Name+`":`) {
			t.Errorf("%s is in noSeedTools; it will never get a rule", td.Name)
		}
	}
	if !strings.Contains(src, "registerMCPToolsWithCompute") {
		t.Error("the registration call vanished")
	}
}

func readNodeSource(t *testing.T) string {
	t.Helper()
	return readSource(t, "node.go")
}

func readSeedSource(t *testing.T) string {
	t.Helper()
	return readSource(t, "wire_seeds.go")
}

func readSource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}
