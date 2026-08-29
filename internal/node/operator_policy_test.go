package node

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

func writePolicy(t *testing.T, dir, tool, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, tool+".toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func nodeWithDirs(dirs ...string) *Node {
	n := &Node{toolRegistry: compute.NewRegistry(), log: slog.New(slog.DiscardHandler)}
	n.cfg.SandboxPolicyDirs = dirs
	return n
}

// The chain was resolved in cmd/lobslaw, logged so operators could
// "verify precedence", and discarded. An operator setting --policy-dir
// saw a line confirming their choice and got no policy at all.
func TestOperatorPoliciesAreApplied(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "git", "name = \"git\"\npaths = [\"/srv/repos:r\"]\n")

	n := nodeWithDirs(dir)
	n.applyOperatorPolicies()

	if got := n.toolRegistry.PolicyFor("git"); got == nil {
		t.Fatal("the operator's policy was not applied to the registry")
	}
}

// Later dirs override earlier ones on the same tool — the ordering the
// loader documents and the registry's last-write-wins implements.
func TestOperatorPoliciesLaterDirWins(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	// Real directories: expandAndCanonicalise DROPS paths that do not
	// exist, on the reasoning that landlock skips them anyway. A policy
	// naming a path that is not there yet therefore grants nothing,
	// which is worth knowing when writing one.
	pathA, pathB := t.TempDir(), t.TempDir()
	writePolicy(t, first, "git", "name = \"git\"\npaths = [\""+pathA+":r\"]\n")
	writePolicy(t, second, "git", "name = \"git\"\npaths = [\""+pathB+":r\"]\n")

	n := nodeWithDirs(first, second)
	n.applyOperatorPolicies()

	got := n.toolRegistry.PolicyFor("git")
	if got == nil {
		t.Fatal("no policy applied")
	}
	if len(got.AllowedPaths) != 1 || got.AllowedPaths[0] != pathB {
		t.Errorf("allowed paths = %v, want only the later dir's %q", got.AllowedPaths, pathB)
	}
}

// No configured dirs is a supported and common deployment, not an
// error, and must not touch the registry.
func TestOperatorPoliciesNoDirsIsANoOp(t *testing.T) {
	n := nodeWithDirs()
	n.applyOperatorPolicies()
	if got := n.toolRegistry.PolicyFor("git"); got != nil {
		t.Error("a policy appeared with no dirs configured")
	}
}

// A directory that does not exist is a no-op, so an operator who
// declares a path before creating it gets a working node.
func TestOperatorPoliciesMissingDirIsANoOp(t *testing.T) {
	n := nodeWithDirs(filepath.Join(t.TempDir(), "absent"))
	n.applyOperatorPolicies()
}

// Boot must survive a malformed file. The policy is lost — which is
// why it is logged — but the node comes up and every other tool keeps
// the policy it was given.
func TestOperatorPoliciesSurviveAMalformedFile(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "broken", "this is not valid toml {{{")
	writePolicy(t, dir, "git", "name = \"git\"\npaths = [\"/srv/repos:r\"]\n")

	n := nodeWithDirs(dir)
	n.applyOperatorPolicies()

	if got := n.toolRegistry.PolicyFor("git"); got == nil {
		t.Error("a malformed sibling prevented a valid policy from loading")
	}
}

// The registry satisfies the sink the loader and watcher both write
// through. If that ever stops being true the wiring breaks silently.
func TestToolRegistrySatisfiesPolicySink(t *testing.T) {
	var _ sandbox.PolicySink = compute.NewRegistry()
}
