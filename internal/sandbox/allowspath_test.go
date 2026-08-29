package sandbox

import "testing"

// A tool with no policy is unconfined BY THIS LAYER — matching
// sandbox.Apply, which is a no-op on a nil Policy. Anything else would
// make the guard chain's last step deny every path-taking tool on a
// node that never wrote a policy file.
func TestAllowsPathIsOpenWithoutAPolicy(t *testing.T) {
	t.Parallel()
	var nilPolicy *Policy
	if !nilPolicy.AllowsPath("/anywhere", AccessRW) {
		t.Error("a nil policy must not confine")
	}
	if !(&Policy{}).AllowsPath("/anywhere", AccessRW) {
		t.Error("an empty policy must not confine")
	}
	// A policy that is seccomp-only is not a filesystem policy.
	// Reading it as deny-all would disable every path-taking tool the
	// moment an operator confined one by syscall.
	syscallOnly := &Policy{NoNewPrivs: true, Seccomp: DefaultSeccompPolicy}
	if !syscallOnly.AllowsPath("/anywhere", AccessR) {
		t.Error("a policy naming no filesystem area must not deny paths")
	}
}

func TestAllowsPathHonoursTheAccessMask(t *testing.T) {
	t.Parallel()
	p := &Policy{Mounts: []PolicyMount{
		{Path: "/workspace", Read: true, Write: true},
		{Path: "/srv/ro", Read: true},
	}}
	cases := []struct {
		path string
		need Access
		want bool
	}{
		{"/workspace/a.txt", AccessR, true},
		{"/workspace/a.txt", AccessRW, true},
		{"/srv/ro/a.txt", AccessR, true},
		{"/srv/ro/a.txt", AccessRW, false}, // read-only means read-only
		{"/srv/ro/a.txt", AccessRWX, false},
		{"/etc/passwd", AccessR, false}, // not named at all
	}
	for _, c := range cases {
		if got := p.AllowsPath(c.path, c.need); got != c.want {
			t.Errorf("AllowsPath(%q, %v) = %v, want %v", c.path, c.need, got, c.want)
		}
	}
}

// The classic boundary leak: a prefix test lets "/workspaces" pass a
// check written for "/workspace".
func TestAllowsPathIsSeparatorAware(t *testing.T) {
	t.Parallel()
	p := &Policy{Mounts: []PolicyMount{{Path: "/workspace", Read: true}}}
	if p.AllowsPath("/workspaces/other/secret", AccessR) {
		t.Error("a sibling directory sharing a name prefix was allowed in")
	}
	if !p.AllowsPath("/workspace", AccessR) {
		t.Error("the root itself should be allowed")
	}
	// .. must not walk out of a permitted root.
	if p.AllowsPath("/workspace/../etc/passwd", AccessR) {
		t.Error("a traversal escaped the permitted root")
	}
}

// A narrow entry inside a wide one has to win, or the answer depends
// on the order somebody wrote the file in.
func TestAllowsPathPrefersTheMostSpecificEntry(t *testing.T) {
	t.Parallel()
	// Deliberately declared widest-first, which is the order that
	// would break a naive first-match implementation.
	p := &Policy{Mounts: []PolicyMount{
		{Path: "/workspace", Read: true, Write: true},
		{Path: "/workspace/secrets", Read: true},
	}}
	if p.AllowsPath("/workspace/secrets/k.pem", AccessRW) {
		t.Error("a narrow read-only entry was shadowed by the wider rw one above it")
	}
	if !p.AllowsPath("/workspace/secrets/k.pem", AccessR) {
		t.Error("the narrow entry should still permit its own mode")
	}
	if !p.AllowsPath("/workspace/build.log", AccessRW) {
		t.Error("the wide entry should still apply outside the narrow one")
	}
}

// The legacy pair has to mean the same thing as Mounts, or a policy
// written either way confines differently.
func TestAllowsPathReadsTheLegacyFields(t *testing.T) {
	t.Parallel()
	p := &Policy{
		AllowedPaths:  []string{"/workspace", "/srv/ro"},
		ReadOnlyPaths: []string{"/srv/ro"},
	}
	if !p.AllowsPath("/workspace/x", AccessRW) {
		t.Error("an AllowedPaths entry should be read-write")
	}
	if p.AllowsPath("/srv/ro/x", AccessRW) {
		t.Error("a ReadOnlyPaths entry accepted a write")
	}
	if !p.AllowsPath("/srv/ro/x", AccessR) {
		t.Error("a ReadOnlyPaths entry should permit reads")
	}
}
