package node

import (
	"fmt"
	"slices"

	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Called by the workforce/computer integration stage after computer setup. It
// grants no policy rules: browser tools traverse the same Executor as other tools.
func (n *Node) registerComputerTools(resolve tools.BrowserScopeResolver) error {
	if !n.cfg.Computer.Enabled || !slices.Contains(n.cfg.Functions, types.FunctionComputeTeams) || n.computer == nil || n.toolRegistry == nil || n.builtinsRegistry == nil {
		return nil
	}
	if err := tools.RegisterBrowserBuiltins(n.builtinsRegistry, n.computer, resolve); err != nil {
		return fmt.Errorf("register browser builtins: %w", err)
	}
	for _, def := range tools.BrowserToolDefs() {
		if err := n.toolRegistry.Register(def); err != nil {
			return fmt.Errorf("register browser tool: %w", err)
		}
	}
	return nil
}
