package node

import (
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/tools"
)

func (n *Node) registerWorkforceTools() error {
	if n.workforce == nil || n.builtinsRegistry == nil || n.toolRegistry == nil {
		return nil
	}
	if e := tools.RegisterWorkforceBuiltins(n.builtinsRegistry, n.workforce); e != nil {
		return e
	}
	for _, def := range tools.WorkforceToolDefs() {
		if e := n.toolRegistry.Register(def); e != nil {
			return fmt.Errorf("register workforce tool %q: %w", def.Name, e)
		}
	}
	return nil
}
