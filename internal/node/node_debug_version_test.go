package node

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// debug_version is the only way the agent can learn what build it is
// running, so it has to carry the build stamp. It used to answer with
// a node ID and enabled functions — nothing a question about versions
// wants, and an invitation to fill the gap from imagination.
func TestDebugVersionReportsTheBuildStamp(t *testing.T) {
	t.Parallel()
	d := &debugInspector{n: &Node{cfg: Config{
		NodeID:    "lobslaw-local",
		Functions: []types.NodeFunction{types.FunctionCompute},
		Version:   "1.4.0",
		Commit:    "43fa295",
		BuildDate: "2026-09-01T09:30:00Z",
	}}}

	got := d.DebugVersion()
	for _, want := range []string{"version=1.4.0", "commit=43fa295", "built=2026-09-01T09:30:00Z", "node_id=lobslaw-local"} {
		if !strings.Contains(got, want) {
			t.Errorf("DebugVersion() = %q; missing %q", got, want)
		}
	}
}

// An unstamped build (go build with no ldflags) must say so. Reporting
// an empty version reads as a missing field; the agent needs to be
// able to tell the user the build does not carry one.
func TestDebugVersionSaysWhenTheBuildIsUnstamped(t *testing.T) {
	t.Parallel()
	d := &debugInspector{n: &Node{cfg: Config{NodeID: "n1"}}}

	got := d.DebugVersion()
	if !strings.Contains(got, "version=unstamped") || !strings.Contains(got, "commit=unknown") {
		t.Errorf("DebugVersion() = %q; want it to name the build as unstamped", got)
	}
}

// A binary built from a tree with uncommitted changes must say so.
// The commit hash is a claim about which source is running, and an
// edited tree makes it a false one — this is the field that stops
// somebody chasing a bug through source the running code never saw.
func TestDebugVersionFlagsADirtyBuild(t *testing.T) {
	t.Parallel()
	d := &debugInspector{n: &Node{cfg: Config{
		NodeID: "n1", Version: "dev", Commit: "abc123", BuildDirty: true,
	}}}

	if got := d.DebugVersion(); !strings.Contains(got, "DIRTY") {
		t.Errorf("DebugVersion() = %q; a dirty build must be marked", got)
	}
}
