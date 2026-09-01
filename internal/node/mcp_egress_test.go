package node

import (
	"log/slog"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

func nodeWithMCP(servers map[string]config.MCPServerConfig) *Node {
	n := &Node{log: slog.New(slog.DiscardHandler)}
	n.cfg.MCP.Servers = servers
	return n
}

// A remote server's URL is its allowlist. Writing the host a second
// time would be a list that can drift from the one that matters.
func TestRemoteServerIsPinnedToItsOwnHost(t *testing.T) {
	t.Parallel()
	got := mcpEgressHosts(nodeWithMCP(map[string]config.MCPServerConfig{
		"kitchenowl": {URL: "https://kitchen.example.net:8080/sse"},
	}))
	if len(got["kitchenowl"]) != 1 || got["kitchenowl"][0] != "kitchen.example.net" {
		t.Fatalf("hosts = %v; want the url's host without its port", got["kitchenowl"])
	}
}

// A stdio server reaches what it declares, and nothing when it
// declares nothing. Empty is the correct default: it makes the
// omission visible as a denial rather than as unbounded access.
func TestStdioServerGetsItsDeclaredNetworks(t *testing.T) {
	t.Parallel()
	got := mcpEgressHosts(nodeWithMCP(map[string]config.MCPServerConfig{
		"minimax": {Command: "uvx", Networks: []string{"api.minimax.chat"}},
		"quiet":   {Command: "some-server"},
	}))
	if len(got["minimax"]) != 1 || got["minimax"][0] != "api.minimax.chat" {
		t.Errorf("minimax hosts = %v", got["minimax"])
	}
	if len(got["quiet"]) != 0 {
		t.Errorf("a server declaring no networks got %v", got["quiet"])
	}
}

// A disabled server should not appear in the ACL at all: a role for
// something that never starts is a rule nobody can account for.
func TestDisabledServersGetNoRole(t *testing.T) {
	t.Parallel()
	got := mcpEgressHosts(nodeWithMCP(map[string]config.MCPServerConfig{
		"off": {URL: "https://example.test/sse", Disabled: true},
	}))
	if _, present := got["off"]; present {
		t.Error("a disabled server was given an egress role")
	}
}

// A url with no host cannot be pinned, and silently allowing it
// would be the one outcome worse than denying it.
func TestUnparseableURLReachesNothing(t *testing.T) {
	t.Parallel()
	got := mcpEgressHosts(nodeWithMCP(map[string]config.MCPServerConfig{
		"broken": {URL: "not-a-url"},
	}))
	if len(got["broken"]) != 0 {
		t.Errorf("hosts = %v; want none", got["broken"])
	}
}
