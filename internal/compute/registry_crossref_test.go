package compute

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func def(name string, recommend, avoid []string) *types.ToolDef {
	return &types.ToolDef{
		Name:             name,
		Path:             BuiltinScheme + name,
		Description:      "does a thing.",
		ParametersSchema: []byte(`{"type":"object"}`),
		RiskTier:         types.RiskReversible,
		RecommendTools:   recommend,
		AvoidTools:       avoid,
	}
}

// The bug this exists to fix: shell_command's description hardcoded
// "prefer read_file, list_files, glob, grep", and disabled_tools could
// switch any of them off — leaving the model chasing a tool it cannot
// call.
func TestCrossRefsNameOnlyRegisteredTools(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	// glob is deliberately never registered.
	for _, d := range []*types.ToolDef{
		def("shell_command", []string{"read_file", "glob", "grep"}, nil),
		def("read_file", nil, nil),
		def("grep", nil, nil),
	} {
		if err := r.Register(d); err != nil {
			t.Fatal(err)
		}
	}

	got, _ := r.Get("shell_command")
	if !strings.Contains(got.Description, "read_file, grep") {
		t.Errorf("live recommendations missing or reordered: %q", got.Description)
	}
	if strings.Contains(got.Description, "glob") {
		t.Errorf("an unregistered tool was recommended: %q", got.Description)
	}
}

// The case the whole line hangs on. "Prefer these where they fit:"
// with nothing after it tells the model something was withheld and
// invites it to guess what.
func TestAnEmptyCrossRefRendersNoSentence(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(def("lonely", []string{"absent"}, []string{"also-absent"})); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("lonely")
	if got.Description != "does a thing." {
		t.Errorf("a fully-filtered cross-ref left residue: %q", got.Description)
	}
	for _, leak := range []string{"Prefer", "Do not use"} {
		if strings.Contains(got.Description, leak) {
			t.Errorf("empty list still rendered its lead-in %q: %q", leak, got.Description)
		}
	}
}

// Author order is a preference ordering — "try glob, then grep" —
// and sorting it would throw that away for tidiness nobody asked for.
func TestCrossRefsKeepAuthorOrder(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	for _, n := range []string{"zebra", "alpha"} {
		if err := r.Register(def(n, nil, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(def("caller", []string{"zebra", "alpha"}, nil)); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("caller")
	if !strings.Contains(got.Description, "zebra, alpha") {
		t.Errorf("author order was not preserved: %q", got.Description)
	}
}

// Both directions render, and independently.
func TestRecommendAndAvoidAreSeparateLines(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	for _, n := range []string{"good", "bad"} {
		if err := r.Register(def(n, nil, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Register(def("both", []string{"good"}, []string{"bad"})); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("both")
	if !strings.Contains(got.Description, "Prefer these where they fit: good.") {
		t.Errorf("recommend line missing: %q", got.Description)
	}
	if !strings.Contains(got.Description, "Do not use these in its place: bad.") {
		t.Errorf("avoid line missing: %q", got.Description)
	}
}

// LLMTools is what actually reaches the provider, so the rendering
// has to survive that path and not only Get.
func TestLLMToolsCarriesCrossRefs(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(def("target", nil, nil)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(def("caller", []string{"target"}, nil)); err != nil {
		t.Fatal(err)
	}
	for _, tool := range r.LLMTools() {
		if tool.Name == "caller" {
			if !strings.Contains(tool.Description, "target") {
				t.Errorf("cross-refs did not reach the wire shape: %q", tool.Description)
			}
			return
		}
	}
	t.Fatal("caller missing from LLMTools")
}
