package compute

import (
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func toolNamed(name string) *types.ToolDef {
	return &types.ToolDef{
		Name:             name,
		Path:             BuiltinScheme + name,
		Description:      "x",
		ParametersSchema: []byte(`{"type":"object"}`),
		RiskTier:         types.RiskReversible,
	}
}

// The default the whole rename exists to establish: a deployment that
// has said nothing does not get a tool that runs commands off the box.
func TestDefaultDisabledTurnsOffTheRemoteFamily(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.SetDisabled(DefaultDisabledTools)

	for _, name := range []string{"remote_ssh", "remote_scp", "remote_anything_later"} {
		if err := r.Register(toolNamed(name)); err != nil {
			t.Fatalf("Register(%q) should be a silent no-op, got %v", name, err)
		}
		if _, ok := r.Get(name); ok {
			t.Errorf("%q was registered despite the default disable list", name)
		}
	}
	// And nothing else is caught by it.
	if err := r.Register(toolNamed("read_file")); err != nil {
		t.Fatalf("Register(read_file): %v", err)
	}
	if _, ok := r.Get("read_file"); !ok {
		t.Error("remote_* must not disable anything outside the family")
	}
}

// An operator writing `disabled_tools = []` means all of them.
func TestExplicitlyEmptyDisablesNothing(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.SetDisabled([]string{})
	if err := r.Register(toolNamed("remote_ssh")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Get("remote_ssh"); !ok {
		t.Error("an explicitly empty disable list should register remote_ssh")
	}
}

// The gate is at the registry precisely so a skill or an MCP server
// cannot introduce a name in a family the operator disabled.
func TestDisabledAppliesToExternalToolsToo(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.SetDisabled([]string{"remote_*"})

	ext := toolNamed("remote_ssh")
	ext.Path = "mcp:someserver:remote_ssh"
	if err := r.RegisterExternal(ext); err != nil {
		t.Fatalf("RegisterExternal: %v", err)
	}
	if _, ok := r.Get("remote_ssh"); ok {
		t.Error("an MCP server slipped a disabled tool into the registry")
	}
	// Replace is the third door and has to be shut as well.
	if err := r.Replace(toolNamed("remote_ssh")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if _, ok := r.Get("remote_ssh"); ok {
		t.Error("Replace bypassed the disable list")
	}
}

// A typo must not strip the agent of everything. Matching nothing is
// recoverable — the operator sees the tool still there and fixes the
// pattern; matching everything is a node that looks broken.
func TestInvalidPatternMatchesNothing(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.SetDisabled([]string{"[unclosed"})
	if r.Disabled("remote_ssh") || r.Disabled("read_file") {
		t.Error("a malformed glob should match nothing, not everything")
	}
}

// Exact names work alongside globs — disabling one tool should not
// require learning glob syntax.
func TestDisabledAcceptsExactNames(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	r.SetDisabled([]string{"shell_command", "remote_*"})
	if !r.Disabled("shell_command") {
		t.Error("an exact name should match")
	}
	if !r.Disabled("remote_ssh") {
		t.Error("a glob alongside an exact name should still match")
	}
	if r.Disabled("read_file") {
		t.Error("an unrelated tool matched")
	}
}
