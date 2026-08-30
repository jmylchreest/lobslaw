package tools

import (
	"slices"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// ShellToolDef spells the name as a literal so the inventory tests'
// source scans can see it; this is what stops that duplication
// drifting from the constant the boot path matches policy files
// against.
func TestTheShellToolNameMatchesItsDefinition(t *testing.T) {
	t.Parallel()
	if got := ShellToolDef().Name; got != ShellToolName {
		t.Errorf("ShellToolDef().Name = %q, ShellToolName = %q", got, ShellToolName)
	}
}

// mountPaths lists the paths a composed policy grants, so assertions
// can talk about coverage rather than slice positions.
func mountPaths(p *sandbox.Policy) []string {
	out := make([]string, 0, len(p.Mounts))
	for _, m := range p.Mounts {
		out = append(out, m.Path)
	}
	return out
}

func mountFor(t *testing.T, p *sandbox.Policy, path string) sandbox.PolicyMount {
	t.Helper()
	for _, m := range p.Mounts {
		if m.Path == path {
			return m
		}
	}
	t.Fatalf("policy grants no %s; it grants %v", path, mountPaths(p))
	return sandbox.PolicyMount{}
}

// The floor had no /dev at all, so `cmd 2>/dev/null` — ordinary shell,
// not a privileged act — failed with a bare "Permission denied" that
// reads like a broken command rather than a policy decision.
func TestTheShellFloorGrantsTheDevicesOrdinaryCommandsAssume(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy([]sandbox.PolicyMount{{Path: "/ws", Read: true, Write: true}}, nil)

	devNull := mountFor(t, policy, "/dev/null")
	if !devNull.Read || !devNull.Write {
		t.Errorf("/dev/null = %+v; redirects need it writable", devNull)
	}
	for _, path := range []string{"/dev/zero", "/dev/urandom", "/dev/random", "/dev/full"} {
		if m := mountFor(t, policy, path); !m.Read {
			t.Errorf("%s = %+v; want readable", path, m)
		}
	}
}

// A tool with no terminal that waits for one waits forever. Failing is
// the better outcome, so the floor withholds /dev/tty on purpose and
// this pins that rather than leaving it to look like an oversight.
func TestTheShellFloorWithholdsTheTerminal(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy([]sandbox.PolicyMount{{Path: "/ws", Read: true}}, nil)
	if slices.Contains(mountPaths(policy), "/dev/tty") {
		t.Error("/dev/tty is granted; a command that prompts would hang instead of failing")
	}
}

func TestTheShellFloorStillGrantsTheLoaderAndBinaries(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy([]sandbox.PolicyMount{{Path: "/ws", Read: true}}, nil)
	granted := mountPaths(policy)
	for _, path := range []string{"/lib", "/lib64", "/usr/bin", "/bin", "/etc", "/tmp"} {
		if !slices.Contains(granted, path) {
			t.Errorf("floor lost %s; the shell cannot exec without it", path)
		}
	}
	if !slices.Contains(granted, "/ws") {
		t.Error("storage mount /ws was dropped")
	}
}

// The point of item 3: a path the operator wants reachable is granted
// in a file, not by editing Go and rebuilding.
func TestAnOperatorOverlayWidensTheShellSandbox(t *testing.T) {
	t.Parallel()
	overlay := &sandbox.Policy{
		Mounts:        []sandbox.PolicyMount{{Path: "/srv/data", Read: true}},
		AllowedPaths:  []string{"/opt/tool", "/var/cache/tool"},
		ReadOnlyPaths: []string{"/opt/tool"},
	}
	policy := composeShellPolicy([]sandbox.PolicyMount{{Path: "/ws", Read: true}}, overlay)

	if m := mountFor(t, policy, "/srv/data"); !m.Read {
		t.Errorf("/srv/data = %+v, want readable", m)
	}
	if !slices.Equal(policy.AllowedPaths, overlay.AllowedPaths) {
		t.Errorf("AllowedPaths = %v, want %v", policy.AllowedPaths, overlay.AllowedPaths)
	}
	if !slices.Equal(policy.ReadOnlyPaths, overlay.ReadOnlyPaths) {
		t.Errorf("ReadOnlyPaths = %v, want %v", policy.ReadOnlyPaths, overlay.ReadOnlyPaths)
	}
}

// The floor's own roots survive whatever the file says: an overlay
// that dropped /lib would leave a shell that cannot exec /bin/sh,
// which is not a policy anyone means to write.
//
// This is about the floor's entries, not about every path beneath
// them — an operator naming a path nested inside one can still tighten
// it, which Landlock resolves in favour of the deeper rule.
func TestAnOperatorOverlayCannotRemoveTheFloor(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy(nil, &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/srv/data", Read: true}},
	})
	granted := mountPaths(policy)
	for _, path := range []string{"/lib", "/bin", "/dev/null"} {
		if !slices.Contains(granted, path) {
			t.Errorf("overlay removed %s from the floor", path)
		}
	}
	if !policy.NoNewPrivs {
		t.Error("overlay cleared NoNewPrivs; Landlock requires it")
	}
}

func TestFieldsShellCommandCannotHonourAreNamedNotSwallowed(t *testing.T) {
	t.Parallel()
	unsupported := UnsupportedShellPolicyFields(&sandbox.Policy{
		Mounts:           []sandbox.PolicyMount{{Path: "/srv", Read: true}},
		Namespaces:       sandbox.NamespaceSet{Network: true},
		NetworkAllowCIDR: []string{"10.0.0.0/8"},
	})
	joined := strings.Join(unsupported, " ")
	if !strings.Contains(joined, "namespaces") {
		t.Errorf("unsupported = %v; an operator would think namespaces took effect", unsupported)
	}
	if !strings.Contains(joined, "network") {
		t.Errorf("unsupported = %v; an operator would think egress was restricted", unsupported)
	}
}

func TestAnEnforceableOverlayNamesNothingUnsupported(t *testing.T) {
	t.Parallel()
	if unsupported := UnsupportedShellPolicyFields(&sandbox.Policy{
		Mounts:  []sandbox.PolicyMount{{Path: "/srv", Read: true}},
		Seccomp: sandbox.SeccompPolicy{Deny: []string{"chroot"}},
	}); len(unsupported) != 0 {
		t.Errorf("unsupported = %v, want none", unsupported)
	}
}

func TestNoOverlayNamesNothingUnsupported(t *testing.T) {
	t.Parallel()
	if unsupported := UnsupportedShellPolicyFields(nil); unsupported != nil {
		t.Errorf("unsupported = %v, want nil", unsupported)
	}
}

// A denial is indistinguishable from an ordinary permissions problem
// in the text, so without this the model's next move is sudo or a
// copy — neither of which can work against a path-based denial.
func TestAFailedCommandThatLooksLikeADenialIsExplained(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy([]sandbox.PolicyMount{
		{Path: "/ws", Read: true, Write: true},
		{Path: "/ro", Read: true},
	}, nil)

	note := sandboxDenialNote(policy, 2,
		[]byte("ls: cannot open directory '/srv': Permission denied\n"))
	if note == nil {
		t.Fatal("no note; the model cannot tell a sandbox denial from a chmod problem")
	}
	if !slices.Contains(note.Writable, "/ws") {
		t.Errorf("writable = %v, want /ws listed", note.Writable)
	}
	if !slices.Contains(note.Readable, "/ro") {
		t.Errorf("readable = %v, want /ro listed", note.Readable)
	}
	if !strings.Contains(note.Note, "do not retry") {
		t.Errorf("note does not tell the model to stop retrying: %q", note.Note)
	}
	if !strings.Contains(note.Note, "policy.d/"+ShellToolName+".toml") {
		t.Errorf("note does not say where the grant would go: %q", note.Note)
	}
}

func TestTheOverlaysOwnPathsAreListedInTheDenialNote(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy(nil, &sandbox.Policy{
		AllowedPaths:  []string{"/opt/ro", "/opt/rw"},
		ReadOnlyPaths: []string{"/opt/ro"},
	})
	note := sandboxDenialNote(policy, 1, []byte("open /x: permission denied"))
	if note == nil {
		t.Fatal("no note")
	}
	if !slices.Contains(note.Readable, "/opt/ro") || !slices.Contains(note.Writable, "/opt/rw") {
		t.Errorf("note = %+v; operator-granted paths must be listed too", note)
	}
}

func TestNothingIsExplainedWhenThereIsNothingToExplain(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy([]sandbox.PolicyMount{{Path: "/ws", Read: true}}, nil)
	cases := []struct {
		name     string
		policy   *sandbox.Policy
		exitCode int
		stderr   string
	}{
		{"unsandboxed run has no sandbox to blame", nil, 2, "Permission denied"},
		{"successful command needs no help", policy, 0, "Permission denied"},
		{"ordinary failure is not a denial", policy, 1, "no such file or directory"},
		{"clean failure is not a denial", policy, 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if note := sandboxDenialNote(tc.policy, tc.exitCode, []byte(tc.stderr)); note != nil {
				t.Errorf("note = %+v, want none", note)
			}
		})
	}
}

func TestDenialsAreRecognisedInTheShapesTheKernelProduces(t *testing.T) {
	t.Parallel()
	for _, stderr := range []string{
		"ls: cannot open directory '/srv': Permission denied",
		"sh: 1: cannot create /dev/null: Permission denied",
		"mount: Operation not permitted",
		"Error: EACCES: permission denied, open '/etc/x'",
		"write failed: EPERM",
	} {
		if !looksLikeDenial([]byte(stderr)) {
			t.Errorf("not recognised as a denial: %q", stderr)
		}
	}
}

// A policy.d seccomp_deny REPLACES the baseline for an ordinary tool.
// For the one tool that runs arbitrary commands, that would let a
// one-line file drop ptrace, mount, bpf and keyctl — a narrowing
// dressed as a setting.
func TestAnOperatorSeccompListCannotDropTheBaseline(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy(nil, &sandbox.Policy{
		Mounts:  []sandbox.PolicyMount{{Path: "/srv", Read: true}},
		Seccomp: sandbox.SeccompPolicy{Deny: []string{"read"}},
	})
	for _, name := range []string{"ptrace", "mount", "bpf", "keyctl", "init_module"} {
		if !slices.Contains(policy.Seccomp.Deny, name) {
			t.Errorf("%q is no longer denied; the overlay narrowed the baseline", name)
		}
	}
	if !slices.Contains(policy.Seccomp.Deny, "read") {
		t.Error("the operator's own addition was dropped")
	}
}

// With no seccomp in the file, the zero value is left for Normalise to
// fill with the default rather than pinned here.
func TestAnOverlayWithoutSeccompLeavesTheDefaultToNormalise(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy(nil, &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/srv", Read: true}},
	})
	before := slices.Clone(policy.Seccomp.Deny)
	policy.Normalise()
	if len(before) != 0 && !slices.Equal(before, sandbox.DefaultSeccompPolicy.Deny) {
		t.Errorf("Seccomp = %v before Normalise; want zero or the baseline", before)
	}
	for _, name := range []string{"ptrace", "mount"} {
		if !slices.Contains(policy.Seccomp.Deny, name) {
			t.Errorf("%q not denied after Normalise", name)
		}
	}
}

// Same path from two sources must not let the later one win: the
// floor's /tmp is writable and an operator naming /tmp:r must not
// take that away.
func TestAnOverlayNamingAFloorPathCannotNarrowIt(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy(nil, &sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/tmp", Read: true}},
	})
	if m := mountFor(t, policy, "/tmp"); !m.Write {
		t.Errorf("/tmp = %+v; the floor's write access was narrowed away", m)
	}
}

func TestTheComposedPolicyHasNoDuplicatePaths(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy(
		[]sandbox.PolicyMount{{Path: "/tmp", Read: true, Write: true}},
		&sandbox.Policy{Mounts: []sandbox.PolicyMount{{Path: "/tmp", Read: true}}},
	)
	seen := map[string]bool{}
	for _, m := range policy.Mounts {
		if seen[m.Path] {
			t.Errorf("%s appears twice; merging is what keeps the floor from being replaced", m.Path)
		}
		seen[m.Path] = true
	}
}

// The accessor hands out security-critical state. If it aliased, any
// caller holding the result could widen the live sandbox.
func TestTheOverlayAccessorDoesNotAliasLiveState(t *testing.T) {
	t.Cleanup(func() { SetShellPolicyOverlay(nil) })
	SetShellPolicyOverlay(&sandbox.Policy{
		Mounts: []sandbox.PolicyMount{{Path: "/srv", Read: true}},
	})

	got := ShellPolicyOverlay()
	got.Mounts = append(got.Mounts, sandbox.PolicyMount{Path: "/", Read: true, Write: true})
	got.Mounts[0].Write = true

	fresh := ShellPolicyOverlay()
	if len(fresh.Mounts) != 1 {
		t.Fatalf("mounts = %+v; a caller appended to the live policy", fresh.Mounts)
	}
	if fresh.Mounts[0].Write {
		t.Error("a caller turned a read-only grant writable through the accessor")
	}
}

// The floor is described in prose instead of listed: enumerating
// /lib and /usr/bin back to the model is noise it cannot act on, and
// it hands the provider more of the layout than the answer needs.
func TestTheDenialNoteListsActionableRootsNotTheFloor(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy([]sandbox.PolicyMount{
		{Path: "/srv/ws", Read: true, Write: true},
	}, nil)
	note := sandboxDenialNote(policy, 2, []byte("Permission denied"))
	if note == nil {
		t.Fatal("no note")
	}
	listed := slices.Concat(note.Readable, note.Writable)
	for _, floor := range []string{"/lib", "/usr/bin", "/etc", "/tmp", "/dev/null"} {
		if slices.Contains(listed, floor) {
			t.Errorf("%s is enumerated; the floor should be summarised in prose", floor)
		}
	}
	if !slices.Contains(note.Writable, "/srv/ws") {
		t.Errorf("writable = %v; the actionable root is missing", note.Writable)
	}
	if !strings.Contains(note.Note, "system paths") && !strings.Contains(note.Note, "/usr/bin") {
		t.Errorf("note does not say the system paths are readable: %q", note.Note)
	}
}

// For an ordinary tool an empty policy file means "explicitly
// unsandboxed". There is deliberately no such switch for the one tool
// that runs arbitrary commands: an empty file leaves the floor in
// place rather than disarming it.
func TestAnEmptyOverlayDoesNotDisarmTheSandbox(t *testing.T) {
	t.Parallel()
	policy := composeShellPolicy(nil, &sandbox.Policy{})
	if policy == nil {
		t.Fatal("an empty policy file turned the sandbox off")
	}
	granted := mountPaths(policy)
	for _, path := range []string{"/lib", "/bin", "/dev/null"} {
		if !slices.Contains(granted, path) {
			t.Errorf("floor lost %s", path)
		}
	}
	if !policy.NoNewPrivs {
		t.Error("NoNewPrivs cleared")
	}
}
