package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/internal/tools"
)

// shellPolicyNode is nodeWithDirs plus the cleanup the shell overlay
// needs: it is package-level state in internal/tools, so a test that
// installs one has to take it back out.
func shellPolicyNode(t *testing.T, dirs ...string) *Node {
	t.Helper()
	t.Cleanup(func() { tools.SetShellPolicyOverlay(nil) })
	return nodeWithDirs(dirs...)
}

// shell_command is a builtin: it dispatches in-process and never
// reaches the executor's exec path, so a policy handed to
// Registry.SetPolicy is stored and never consulted. The one tool an
// operator most wants to confine was the one their file could not
// reach.
func TestTheShellPolicyGoesToTheOverlayNotTheRegistry(t *testing.T) {
	// A real directory: the loader drops rules whose path doesn't
	// resolve, so it won't grant access to something that could later
	// appear as a symlink to /etc/passwd.
	granted := t.TempDir()
	dir := t.TempDir()
	writePolicy(t, dir, tools.ShellToolName,
		"name = \"shell_command\"\npaths = [\""+granted+":rw\"]\n")

	n := shellPolicyNode(t, dir)
	n.applyOperatorPolicies()

	if p := n.toolRegistry.PolicyFor(tools.ShellToolName); p != nil {
		t.Error("the shell policy went to the registry, where the builtin path never reads it")
	}
	if !overlayGrants(granted) {
		t.Errorf("the overlay never received it; it grants %v", overlayPaths())
	}
}

// Same stance as a malformed file: loud, and the tool keeps what it
// had. An operator who wrote network_filter believes egress is
// restricted, so the node must not quietly enforce a policy with that
// part missing.
func TestAShellPolicyDeclaringAControlWeCannotEnforceIsDropped(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, tools.ShellToolName,
		"name = \"shell_command\"\nnetwork_allow_cidr = [\"10.0.0.0/8\"]\n")

	n := shellPolicyNode(t, dir)
	n.applyOperatorPolicies()

	if tools.ShellPolicyOverlay() != nil {
		t.Error("a policy naming an unenforceable control was installed anyway")
	}
}

// The same field on an ordinary tool is fine — it reaches the registry
// and the executor's exec path honours what it can.
func TestTheSameFieldOnAnOrdinaryToolIsAccepted(t *testing.T) {
	dir := t.TempDir()
	writePolicy(t, dir, "curl", "name = \"curl\"\nnetwork_allow_cidr = [\"10.0.0.0/8\"]\n")

	n := nodeWithDirs(dir)
	n.applyOperatorPolicies()

	if n.toolRegistry.PolicyFor("curl") == nil {
		t.Error("curl.toml loaded nothing")
	}
}

// The watcher existed, was tested, and was called by nothing, so the
// hot-reload behaviour SANDBOX.md documents was a claim the code did
// not make.
func TestAnEditedPolicyTakesEffectWithoutARestart(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	dir := t.TempDir()
	writePolicy(t, dir, "git", "name = \"git\"\npaths = [\""+first+":rw\"]\n")

	n := nodeWithDirs(dir)
	n.applyOperatorPolicies()
	n.startSandboxWatcher(testCtx(t))

	writePolicy(t, dir, "git", "name = \"git\"\npaths = [\""+second+":rw\"]\n")

	waitFor(t, func() bool {
		p := n.toolRegistry.PolicyFor("git")
		return p != nil && hasMount(p.Mounts, second)
	}, "the edited policy never took effect")
}

// A deleted file clears the policy so the fleet default takes over
// again, rather than leaving the last-loaded grant in force forever.
func TestADeletedPolicyFileClearsTheTool(t *testing.T) {
	granted := t.TempDir()
	dir := t.TempDir()
	writePolicy(t, dir, "git", "name = \"git\"\npaths = [\""+granted+":rw\"]\n")

	n := nodeWithDirs(dir)
	n.applyOperatorPolicies()
	n.startSandboxWatcher(testCtx(t))

	if err := os.Remove(filepath.Join(dir, "git.toml")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return n.toolRegistry.PolicyFor("git") == nil },
		"the removed policy stayed in force")
}

// Reloads route through the same sink as boot, so shell_command
// reaches the overlay there too. Two paths doing that job is how they
// come to disagree about which tool gets what.
func TestAnEditedShellPolicyReachesTheOverlay(t *testing.T) {
	granted := t.TempDir()
	dir := t.TempDir()
	writePolicy(t, dir, "other", "name = \"other\"\npaths = [\"/tmp:r\"]\n")

	n := shellPolicyNode(t, dir)
	n.applyOperatorPolicies()
	n.startSandboxWatcher(testCtx(t))

	writePolicy(t, dir, tools.ShellToolName,
		"name = \"shell_command\"\npaths = [\""+granted+":rw\"]\n")

	waitFor(t, func() bool { return overlayGrants(granted) },
		"the new shell policy never reached the overlay")
	if n.toolRegistry.PolicyFor(tools.ShellToolName) != nil {
		t.Error("it went to the registry too, where the builtin path never reads it")
	}
}

// Mid-run an unenforceable edit is dropped and whatever was already in
// force stands. Widening to nothing because the new file was bad is
// the wrong direction.
func TestAnUnenforceableEditLeavesThePreviousPolicyStanding(t *testing.T) {
	granted := t.TempDir()
	dir := t.TempDir()
	writePolicy(t, dir, tools.ShellToolName,
		"name = \"shell_command\"\npaths = [\""+granted+":rw\"]\n")

	n := shellPolicyNode(t, dir)
	n.applyOperatorPolicies()

	n.policySink().SetPolicy(tools.ShellToolName, &sandbox.Policy{
		NetworkAllowCIDR: []string{"10.0.0.0/8"},
	})

	if !overlayGrants(granted) {
		t.Error("the rejected edit replaced a policy that was enforceable")
	}
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// overlayPaths reports what the installed shell overlay grants, read
// back through the same package-level state the builtin consults.
func overlayPaths() []string {
	policy := tools.ShellPolicyOverlay()
	if policy == nil {
		return nil
	}
	out := make([]string, 0, len(policy.Mounts)+len(policy.AllowedPaths))
	for _, m := range policy.Mounts {
		out = append(out, m.Path)
	}
	return append(out, policy.AllowedPaths...)
}

func overlayGrants(path string) bool {
	for _, p := range overlayPaths() {
		if p == path {
			return true
		}
	}
	return false
}

func hasMount(mounts []sandbox.PolicyMount, want string) bool {
	for _, m := range mounts {
		if m.Path == want {
			return true
		}
	}
	return false
}

// waitFor polls until cond holds or the deadline passes. The watcher
// debounces filesystem events, so the reload is deliberately not
// immediate.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}
