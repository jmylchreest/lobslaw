package sandbox

import (
	"slices"
	"testing"
)

func TestPolicyValidateRejectsRelativeAllowedPath(t *testing.T) {
	t.Parallel()
	p := &Policy{AllowedPaths: []string{"relative/foo"}}
	if err := p.Validate(); err == nil {
		t.Fatal("relative AllowedPaths entry should be rejected")
	}
}

func TestPolicyValidateRequiresReadOnlyInAllowed(t *testing.T) {
	t.Parallel()
	p := &Policy{
		AllowedPaths:  []string{"/a"},
		ReadOnlyPaths: []string{"/b"}, // not in AllowedPaths
	}
	if err := p.Validate(); err == nil {
		t.Fatal("ReadOnlyPaths not subset of AllowedPaths should be rejected")
	}
}

func TestPolicyValidateAcceptsReadOnlySubset(t *testing.T) {
	t.Parallel()
	p := &Policy{
		AllowedPaths:  []string{"/a", "/b"},
		ReadOnlyPaths: []string{"/a"},
	}
	if err := p.Validate(); err != nil {
		t.Errorf("valid subset should pass: %v", err)
	}
}

func TestPolicyValidateRejectsNegativeQuotas(t *testing.T) {
	t.Parallel()
	p := &Policy{CPUQuota: -1}
	if err := p.Validate(); err == nil {
		t.Error("negative CPUQuota should be rejected")
	}
	p = &Policy{MemoryLimitMB: -1}
	if err := p.Validate(); err == nil {
		t.Error("negative MemoryLimitMB should be rejected")
	}
}

func TestPolicyNormaliseAppliesDefaultSeccomp(t *testing.T) {
	t.Parallel()
	p := &Policy{Namespaces: NamespaceSet{User: true}}
	p.Normalise()
	if !p.Seccomp.HasRules() {
		t.Error("Normalise should populate DefaultSeccompPolicy when Seccomp was zero")
	}
	// Spot-check: ptrace should always be in the default deny.
	if !slices.Contains(p.Seccomp.Deny, "ptrace") {
		t.Error("default seccomp deny should include ptrace")
	}
}

func TestPolicyNormaliseForcesNoNewPrivsWhenSandboxed(t *testing.T) {
	t.Parallel()
	p := &Policy{Namespaces: NamespaceSet{User: true}}
	p.Normalise()
	if !p.NoNewPrivs {
		t.Error("Normalise should set NoNewPrivs when any sandboxing is enabled")
	}
}

func TestPolicyNormaliseEmptyPolicyStaysEmpty(t *testing.T) {
	t.Parallel()
	p := &Policy{}
	p.Normalise()
	if p.NoNewPrivs {
		t.Error("zero-value Policy should stay zero-value after Normalise (no sandbox at all)")
	}
	if p.Seccomp.HasRules() {
		t.Error("zero-value Policy should not get default seccomp (no sandbox at all)")
	}
}

func TestNamespaceSetEnabled(t *testing.T) {
	t.Parallel()
	if (NamespaceSet{}).Enabled() {
		t.Error("empty NamespaceSet should report disabled")
	}
	if !(NamespaceSet{User: true}).Enabled() {
		t.Error("NamespaceSet{User: true} should report enabled")
	}
	if !(NamespaceSet{Mount: true}).Enabled() {
		t.Error("NamespaceSet{Mount: true} should report enabled")
	}
}

func TestDefaultSeccompPolicyHasCriticalDenies(t *testing.T) {
	t.Parallel()
	// Sanity check the must-have entries. If any of these get removed
	// from DefaultSeccompPolicy, the sandbox is materially weaker —
	// force an explicit decision (update test + decision record).
	critical := []string{
		"ptrace",                        // inter-process memory access
		"unshare", "setns", "pivot_root", // namespace escape
		"mount", "umount", "umount2",    // filesystem manipulation
		"init_module", "finit_module",   // kernel-module load
		"kexec_load",                    // kernel replacement
		"bpf",                           // new kernel attack surface
		"keyctl",                        // keyring manipulation
	}
	for _, name := range critical {
		if !slices.Contains(DefaultSeccompPolicy.Deny, name) {
			t.Errorf("DefaultSeccompPolicy must deny %q (weakening removed it)", name)
		}
	}
}

func TestSeccompPolicyIsZero(t *testing.T) {
	t.Parallel()
	if !(SeccompPolicy{}).IsZero() {
		t.Error("zero-value SeccompPolicy should report IsZero=true")
	}
	if (SeccompPolicy{Deny: []string{}}).IsZero() {
		t.Error("explicit empty Deny should NOT report IsZero (caller wants no rules)")
	}
}
