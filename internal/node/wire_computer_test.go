package node

import (
	"context"
	"testing"

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
