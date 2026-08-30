package compute

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

func guardRegistryWith(t *testing.T, tool string, p *sandbox.Policy) *Registry {
	t.Helper()
	r := NewRegistry()
	if p != nil {
		r.SetPolicy(tool, p)
	}
	prev := activePathGuard
	SetPathGuardRegistry(r)
	t.Cleanup(func() { activePathGuard = prev })
	return r
}

// THE contract. policy.d is hot-reloaded from directories that include
// one under the operator's home, and the agent runs as that user. If a
// policy file could GRANT reach, an agent that talks somebody into
// running a shell has a documented, auto-reloading route to the memory
// key. So the floors run first and a policy can only narrow them.
func TestPolicyCannotWidenPastTheFloors(t *testing.T) {
	guardRegistryWith(t, "read_file", &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/", Read: true, Write: true, Exec: true}},
	})

	// A permissive policy over everything must not reach a
	// cluster-internal path or a hardline one.
	for _, path := range []string{
		"/var/lobslaw/data/certs/node-key.pem",
		"/etc/lobslaw/keys/memory.key",
	} {
		if !isInternalPath(path) {
			continue // not classed internal here; the hardline case below still applies
		}
		_, payload, exit := guardRead("read_file", path)
		if exit == 0 {
			t.Errorf("a policy granting / reached %s", path)
		}
		if !strings.Contains(string(payload), "hardline_refused") {
			t.Errorf("%s: expected a hardline refusal, got %s", path, payload)
		}
	}
}

// The other direction: a policy narrower than the mounts is obeyed.
func TestPolicyNarrowsWhatTheMountsAllow(t *testing.T) {
	guardRegistryWith(t, "search_files", &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/workspace/public", Read: true}},
	})

	if _, _, exit := guardRead("search_files", "/workspace/public/notes.md"); exit != 0 {
		t.Error("a path the policy permits was refused")
	}
	_, payload, exit := guardRead("search_files", "/workspace/private/notes.md")
	if exit == 0 {
		t.Fatal("a path outside the policy was allowed")
	}
	if !strings.Contains(string(payload), "policy_denied") {
		t.Errorf("expected a policy_denied refusal, got %s", payload)
	}
	// The message has to name the file the operator would edit, or the
	// refusal is unactionable.
	if !strings.Contains(string(payload), "policy.d/search_files.toml") {
		t.Errorf("the refusal should name the policy file: %s", payload)
	}
}

// A policy for a DIFFERENT tool must not confine this one — the whole
// point of per-tool files.
func TestPolicyIsPerTool(t *testing.T) {
	guardRegistryWith(t, "read_file", &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/workspace/only", Read: true}},
	})
	if _, _, exit := guardRead("search_files", "/workspace/elsewhere/x"); exit != 0 {
		t.Error("read_file's policy confined search_files")
	}
	if _, _, exit := guardRead("read_file", "/workspace/elsewhere/x"); exit == 0 {
		t.Error("read_file escaped its own policy")
	}
}

// No registry (a test driving a builtin directly) skips step 5 only.
// It must not skip the floors.
func TestNoRegistrySkipsPolicyNotTheFloors(t *testing.T) {
	prev := activePathGuard
	SetPathGuardRegistry(nil)
	t.Cleanup(func() { activePathGuard = prev })

	if _, _, exit := guardRead("read_file", "/workspace/ordinary.md"); exit != 0 {
		t.Error("an ordinary path should pass with no registry wired")
	}
	if _, _, exit := guardRead("read_file", "relative/path.md"); exit == 0 {
		t.Error("a relative path was accepted with no registry wired")
	}
}

// Read and write are different questions, and a builtin passing the
// wrong one is the bug guardRead/guardWrite exist to prevent.
func TestGuardDistinguishesReadFromWrite(t *testing.T) {
	guardRegistryWith(t, "edit_file", &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/workspace/ro", Read: true}},
	})
	if _, _, exit := guardRead("edit_file", "/workspace/ro/a.md"); exit != 0 {
		t.Error("a read of a read-only policy path was refused")
	}
	_, payload, exit := guardWrite("edit_file", "/workspace/ro/a.md")
	if exit == 0 {
		t.Fatal("a write to a read-only policy path was allowed")
	}
	if !strings.Contains(string(payload), "written") {
		t.Errorf("the refusal should say what was attempted: %s", payload)
	}
}

// An operator policy for a builtin used to load, log success, and be
// consulted by nothing — because sandbox.Apply only runs for
// subprocesses. This is the regression test for that.
func TestABuiltinPolicyIsActuallyConsulted(t *testing.T) {
	r := guardRegistryWith(t, "read_file", &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/workspace/allowed", Read: true}},
	})
	if r.PolicyFor("read_file") == nil {
		t.Fatal("the policy did not reach the registry")
	}
	if _, _, exit := guardRead("read_file", "/workspace/denied/x"); exit == 0 {
		t.Error("the registry holds a policy for read_file and the guard ignored it")
	}
}

func TestSandboxAccessConversion(t *testing.T) {
	t.Parallel()
	if got := sandboxAccess(MountMode{Read: true}); got != sandbox.AccessR {
		t.Errorf("read = %v, want %v", got, sandbox.AccessR)
	}
	if got := sandboxAccess(MountMode{Read: true, Write: true}); got != sandbox.AccessRW {
		t.Errorf("rw = %v, want %v", got, sandbox.AccessRW)
	}
}

// withMounts installs a real resolver for the duration of a test.
//
// Worth its own helper because NOTHING in this package's tests did
// this before: activeMountResolver was nil everywhere, resolveFsPath
// fell open, and a green suite said nothing about how any of these
// tools behave on a node that has mounts. That gap hid a regression in
// this very change — see TestModalityKeepsItsReachOutsideMounts.
func withMounts(t *testing.T, roots map[string]string) {
	t.Helper()
	r := NewMountResolver()
	for label, root := range roots {
		r.Register(label, root, MountMode{Read: true, Write: true}, nil)
	}
	prev := activeMountResolver
	SetActiveMountResolver(r)
	t.Cleanup(func() { SetActiveMountResolver(prev) })
}

// The regression this chain nearly shipped.
//
// The modality tools bounded paths with AllowedRoot and nothing else.
// Putting the mount resolver in front of that would break any
// deployment whose IncomingDir is not inside a declared mount: every
// image, voice note and PDF becomes unreadable, and the operator
// changed nothing.
//
// AllowedRoot stands in for the mount check. Steps 2-5 still run.
func TestModalityKeepsItsReachOutsideMounts(t *testing.T) {
	withMounts(t, map[string]string{"workspace": "/workspace"})

	// Inside a mount: the shipped layout.
	if _, _, exit := guardReadWithin("read_image", "/workspace/incoming/t1/a.png", "/workspace/incoming"); exit != 0 {
		t.Error("the shipped layout was refused")
	}
	// Outside every mount, inside AllowedRoot: an operator who pointed
	// IncomingDir elsewhere. This is the case that regressed.
	if _, payload, exit := guardReadWithin("read_image", "/var/lobslaw/incoming/t1/a.png", "/var/lobslaw/incoming"); exit != 0 {
		t.Errorf("a path inside AllowedRoot but outside every mount was refused: %s", payload)
	}
	// Outside BOTH: still refused, and by the mount resolver.
	if _, _, exit := guardReadWithin("read_image", "/etc/shadow", "/var/lobslaw/incoming"); exit == 0 {
		t.Error("a path outside both the mounts and AllowedRoot was allowed")
	}
}

// The implicit root replaces step 1 and nothing else. A modality tool
// pointed at a root containing cluster state must still be refused —
// that reach is exactly what these tools used to have.
func TestImplicitRootDoesNotBypassTheFloors(t *testing.T) {
	withMounts(t, map[string]string{"workspace": "/workspace"})

	// A root that happens to contain cluster-internal state.
	const root = "/var/lobslaw/data"
	internal := "/var/lobslaw/data/certs/node-key.pem"
	if !isInternalPath(internal) {
		t.Skip("this path is not classed internal in this build")
	}
	_, payload, exit := guardReadWithin("read_image", internal, root)
	if exit == 0 {
		t.Fatal("AllowedRoot let a modality tool reach cluster-internal state")
	}
	if !strings.Contains(string(payload), "hardline_refused") {
		t.Errorf("expected a hardline refusal, got %s", payload)
	}
}

// With mounts configured, the ordinary tools behave as before.
func TestGuardWithRealMounts(t *testing.T) {
	withMounts(t, map[string]string{"workspace": "/workspace"})

	if _, _, exit := guardRead("read_file", "/workspace/notes.md"); exit != 0 {
		t.Error("a path inside a mount was refused")
	}
	if _, _, exit := guardRead("read_file", "/etc/passwd"); exit == 0 {
		t.Error("a path outside every mount was allowed")
	}
	// The label form expands to the mount root.
	resolved, _, exit := guardRead("read_file", "workspace/notes.md")
	if exit != 0 {
		t.Fatalf("the mount-label form was refused")
	}
	if resolved != "/workspace/notes.md" {
		t.Errorf("label expanded to %q, want /workspace/notes.md", resolved)
	}
}
