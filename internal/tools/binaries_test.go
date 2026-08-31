package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/compute"
)

func binaryTools(t *testing.T, decls map[string]BinaryDeclaration) (install, list compute.BuiltinFunc) {
	t.Helper()
	b := NewBuiltins()
	// A non-nil satisfier is needed to register. The paths asserted
	// below refuse before reaching it, which is the point: an
	// undeclared name must never get as far as an installer.
	if err := RegisterBinariesBuiltins(b, BinariesConfig{
		Satisfier:    &binaries.Satisfier{},
		Declarations: decls,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	install, _ = b.Get("binary_install")
	list, _ = b.Get("binary_list")
	return install, list
}

// The declarations map is an allowlist, and that is the security
// property worth pinning: the model chooses the name, so anything not
// declared by the operator must be refused before an installer runs.
func TestBinaryInstallRefusesAnUndeclaredName(t *testing.T) {
	t.Parallel()

	install, _ := binaryTools(t, map[string]BinaryDeclaration{
		"ripgrep": {Name: "ripgrep"},
	})
	out, code, err := install(context.Background(), map[string]string{"name": "curl|sh"})
	if err == nil && code == 0 {
		t.Fatalf("an undeclared binary was accepted: %s", out)
	}
	// The refusal should name what IS available, or the model retries
	// blind against a list it cannot see.
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if !strings.Contains(msg+string(out), "ripgrep") {
		t.Errorf("the refusal does not say what is declared: err=%v out=%s", err, out)
	}
}

func TestBinaryInstallRequiresAName(t *testing.T) {
	t.Parallel()

	install, _ := binaryTools(t, map[string]BinaryDeclaration{"ripgrep": {Name: "ripgrep"}})
	for _, n := range []string{"", "   "} {
		if _, code, err := install(context.Background(), map[string]string{"name": n}); err == nil || code == 0 {
			t.Errorf("name %q was accepted (code=%d)", n, code)
		}
	}
}

// binary_list is how the model learns what it may ask for, so it has
// to report the operator's declarations rather than the host's PATH.
func TestBinaryListReportsTheDeclarations(t *testing.T) {
	t.Parallel()

	_, list := binaryTools(t, map[string]BinaryDeclaration{
		"ripgrep": {Name: "ripgrep"},
		"jq":      {Name: "jq"},
	})
	out, code, err := list(context.Background(), nil)
	if err != nil || code != 0 {
		t.Fatalf("binary_list: code=%d err=%v", code, err)
	}
	for _, want := range []string{"ripgrep", "jq"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("binary_list omits %s: %s", want, out)
		}
	}
}

// Nothing declared means nothing to install, so the tools do not
// appear. A registered binary_install with an empty allowlist can only
// ever refuse, and advertising it invites the model to keep trying.
func TestBinariesWithNothingDeclaredRegisterNothing(t *testing.T) {
	t.Parallel()

	for _, cfg := range []BinariesConfig{
		{},
		{Satisfier: &binaries.Satisfier{}},
		{Declarations: map[string]BinaryDeclaration{"jq": {Name: "jq"}}},
	} {
		b := NewBuiltins()
		if err := RegisterBinariesBuiltins(b, cfg); err != nil {
			t.Fatalf("register: %v", err)
		}
		if _, ok := b.Get("binary_install"); ok {
			t.Errorf("binary_install registered with satisfier=%v declarations=%d",
				cfg.Satisfier != nil, len(cfg.Declarations))
		}
	}
}
