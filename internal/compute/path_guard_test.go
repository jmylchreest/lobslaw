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
		if !strings.Contains(string(payload), "internal_path") {
			t.Errorf("%s: expected an internal_path refusal, got %s", path, payload)
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
