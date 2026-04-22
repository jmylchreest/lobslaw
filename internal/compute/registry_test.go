package compute

import (
	"errors"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func newTool(name string) *types.ToolDef {
	return &types.ToolDef{
		Name:         name,
		Path:         "/usr/bin/" + name,
		ArgvTemplate: []string{name, "{arg}"},
		RiskTier:     types.RiskReversible,
	}
}

func TestRegistryRegisterAndGet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(newTool("bash")); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get("bash")
	if !ok {
		t.Fatal("tool not found after Register")
	}
	if got.Path != "/usr/bin/bash" {
		t.Errorf("Path = %q, want /usr/bin/bash", got.Path)
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(newTool("bash")); err != nil {
		t.Fatal(err)
	}
	err := r.Register(newTool("bash"))
	if err == nil {
		t.Fatal("expected ErrToolExists on duplicate")
	}
	if !errors.Is(err, ErrToolExists) {
		t.Errorf("error = %v, want ErrToolExists", err)
	}
}

func TestRegistryReplaceOverwrites(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(newTool("bash")); err != nil {
		t.Fatal(err)
	}
	updated := newTool("bash")
	updated.Path = "/custom/bash"
	if err := r.Replace(updated); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("bash")
	if got.Path != "/custom/bash" {
		t.Errorf("Path after Replace = %q, want /custom/bash", got.Path)
	}
}

func TestRegistryRejectsInvalid(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	cases := []struct {
		name string
		tool *types.ToolDef
	}{
		{"nil", nil},
		{"empty name", &types.ToolDef{Path: "/x", RiskTier: types.RiskReversible}},
		{"missing path", &types.ToolDef{Name: "x", RiskTier: types.RiskReversible}},
		{"invalid risk tier", &types.ToolDef{Name: "x", Path: "/x", RiskTier: types.RiskTier("nope")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Register(tc.tool); err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestRegistryAllowsSidecarWithoutPath(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	err := r.Register(&types.ToolDef{
		Name:        "git",
		SidecarOnly: true,
		RiskTier:    types.RiskReversible,
	})
	if err != nil {
		t.Errorf("sidecar-only tools should not require Path: %v", err)
	}
}

func TestRegistryListSortedByName(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	for _, name := range []string{"grep", "awk", "sed", "bash", "cat"} {
		if err := r.Register(newTool(name)); err != nil {
			t.Fatal(err)
		}
	}
	list := r.List()
	if len(list) != 5 {
		t.Fatalf("want 5 tools, got %d", len(list))
	}
	want := []string{"awk", "bash", "cat", "grep", "sed"}
	for i, tool := range list {
		if tool.Name != want[i] {
			t.Errorf("List[%d].Name = %q, want %q", i, tool.Name, want[i])
		}
	}
}

func TestRegistryRemoveIsIdempotent(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(newTool("bash")); err != nil {
		t.Fatal(err)
	}
	r.Remove("bash")
	r.Remove("bash")          // again — no panic, no error
	r.Remove("never-existed") // ditto
	if r.Len() != 0 {
		t.Errorf("Len() = %d, want 0", r.Len())
	}
}

func TestRegistryGetReturnsDefensiveCopy(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	if err := r.Register(newTool("bash")); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("bash")
	got.Path = "/hacked"
	got.ArgvTemplate[0] = "hacked"

	// Fetch again; should be unchanged.
	fresh, _ := r.Get("bash")
	if fresh.Path == "/hacked" {
		t.Error("registry mutated through returned pointer — Get must return a copy")
	}
	if fresh.ArgvTemplate[0] == "hacked" {
		t.Error("ArgvTemplate mutated through returned pointer — slices must be deep-copied")
	}
}

func TestRegistryConcurrentAccess(t *testing.T) {
	t.Parallel()
	r := NewRegistry()
	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for range iterations {
				name := "tool-" + string(rune('a'+id%26))
				_ = r.Replace(newTool(name))
				_, _ = r.Get(name)
				_ = r.List()
				_ = r.Len()
			}
		}(i)
	}
	wg.Wait()
	// No panic and internal state still consistent.
	if r.Len() > goroutines {
		t.Errorf("Len() = %d exceeds goroutine count", r.Len())
	}
}
