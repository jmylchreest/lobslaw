package node

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/mcp"
)

// Admitting MCP tools must not admit a name nothing registered.
func TestSkillAllowedRefusesANameNothingRegistered(t *testing.T) {
	t.Parallel()
	for name, n := range map[string]*Node{
		"no registry, no loader": {},
		"no registry, a loader":  {mcpLoader: mcp.NewLoader(mcp.LoaderConfig{})},
	} {
		if n.skillAllowed(t.Context(), "touchline.whoami") {
			t.Errorf("%s: an unregistered tool was allowed", name)
		}
	}
}
