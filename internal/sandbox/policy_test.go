package sandbox

import (
	"encoding/json"
	"reflect"
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

func TestPolicyNormaliseAppliesDefaultSeccompWhenEnforcementRequested(t *testing.T) {
	t.Parallel()
	// Asking for NoNewPrivs is an active enforcement request, so
	// Normalise fills in sensible seccomp defaults alongside it.
	p := &Policy{NoNewPrivs: true}
	p.Normalise()
	if !p.Seccomp.HasRules() {
		t.Error("Normalise should populate DefaultSeccompPolicy when enforcement is requested")
	}
	if !slices.Contains(p.Seccomp.Deny, "ptrace") {
		t.Error("default seccomp deny should include ptrace")
	}
}

func TestPolicyNormaliseFillsNoNewPrivsForLandlockOrSeccomp(t *testing.T) {
	t.Parallel()
	// AllowedPaths → landlock, and landlock requires NoNewPrivs.
	p := &Policy{AllowedPaths: []string{"/tmp/work"}}
	p.Normalise()
	if !p.NoNewPrivs {
		t.Error("Landlock use should force NoNewPrivs=true via Normalise")
	}

	// Explicit Seccomp rules also imply NoNewPrivs.
	q := &Policy{Seccomp: SeccompPolicy{Deny: []string{"ptrace"}}}
	q.Normalise()
	if !q.NoNewPrivs {
		t.Error("explicit seccomp rules should force NoNewPrivs=true via Normalise")
	}
}

func TestPolicyNormaliseNamespacesAloneDontForceEnforcement(t *testing.T) {
	t.Parallel()
	// Namespaces-only is a valid lightweight isolation mode. Normalise
	// must not auto-enable the reexec-helper path (NoNewPrivs + seccomp)
	// just because a caller asked for a user namespace — otherwise a
	// policy that wanted *only* namespaces would break the helper-less
	// Apply path.
	p := &Policy{Namespaces: NamespaceSet{User: true, Mount: true}}
	p.Normalise()
	if p.NoNewPrivs {
		t.Error("Namespaces-only policy should not auto-enable NoNewPrivs")
	}
	if p.Seccomp.HasRules() {
		t.Error("Namespaces-only policy should not auto-populate seccomp")
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

// TestPolicyJSONRoundTrip guards the wire format used to ferry a
// Policy across the exec boundary into the sandbox-exec helper. If
// JSON tags drift, the helper process will mis-apply the policy.
func TestPolicyJSONRoundTrip(t *testing.T) {
	t.Parallel()
	original := Policy{
		AllowedPaths:  []string{"/tmp/work", "/usr"},
		ReadOnlyPaths: []string{"/usr"},
		EnvWhitelist:  []string{"PATH", "HOME"},
		CPUQuota:      2000,
		MemoryLimitMB: 512,
		Namespaces:    NamespaceSet{User: true, Mount: true, PID: true},
		NoNewPrivs:    true,
		Seccomp:       SeccompPolicy{Deny: []string{"ptrace", "mount"}},
	}
	blob, err := json.Marshal(&original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Policy
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(original, got) {
		t.Fatalf("round-trip mismatch\nwant: %+v\n got: %+v", original, got)
	}
}

// TestPolicyJSONEmptyStaysCompact confirms a zero-value Policy
// serialises to a small blob — important because we pass this
// through an env var with a practical length limit.
func TestPolicyJSONEmptyStaysCompact(t *testing.T) {
	t.Parallel()
	blob, err := json.Marshal(&Policy{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(blob); got != "{}" {
		t.Errorf("zero-value Policy should serialise to {}, got %q", got)
	}
}
