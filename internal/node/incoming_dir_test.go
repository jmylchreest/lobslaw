package node

import (
	"os"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// /workspace exists only inside the container image. As a constant it
// meant a host install could not receive an inbound photograph at all
// — MkdirAll under / fails for an unprivileged process — and the
// read_* builtins refused every path, naming a directory the operator
// had no way to change.

func TestTheIncomingDirDefaultsToTheContainerPath(t *testing.T) {
	t.Parallel()
	n := &Node{cfg: Config{}}
	if got := n.incomingDir(); got != compute.DefaultIncomingDir {
		t.Errorf("incomingDir() = %q, want the default %q", got, compute.DefaultIncomingDir)
	}
}

func TestAConfiguredIncomingDirWins(t *testing.T) {
	t.Parallel()
	n := &Node{cfg: Config{
		Gateway: config.GatewayConfig{IncomingDir: "/srv/lobslaw/incoming"},
	}}
	if got := n.incomingDir(); got != "/srv/lobslaw/incoming" {
		t.Errorf("incomingDir() = %q; the configured value was ignored", got)
	}
}

// THE POINT. The channel WRITES here and the vision builtin only
// READS here. Two settings that had to agree would eventually not,
// and the failure is a file sitting somewhere the agent is forbidden
// to look — which reads as "the model cannot see images" rather than
// as a path mismatch, and sends somebody debugging the wrong thing.
//
// Asserted on the source because the alternative is comparing the
// accessor to itself, which cannot fail. What must hold is that both
// wiring sites go through the ONE accessor rather than reaching for
// the config field or the constant on their own.
func TestBothWiringSitesGoThroughTheOneAccessor(t *testing.T) {
	t.Parallel()
	for _, want := range []struct{ file, field string }{
		{"wire_gateway.go", "IncomingDir:"},       // the writer
		{"wire_compute_tools.go", "AllowedRoot:"}, // the reader
	} {
		src, err := os.ReadFile(want.file)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, want.field) && strings.Contains(line, "n.incomingDir()") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s sets %s without going through n.incomingDir(); "+
				"the writer and the reader can now disagree", want.file, want.field)
		}
	}
}
