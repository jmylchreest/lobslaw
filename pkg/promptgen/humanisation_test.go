package promptgen

import (
	"strings"
	"testing"
)

// The rendering guide illustrates each shape with real tools, so the
// examples have to be tools the model actually has. A rule illustrated
// with tools that are not there teaches the shape by pointing at
// nothing.
func TestHumanisationNamesOnlyRegisteredTools(t *testing.T) {
	t.Parallel()

	mentionable := []string{
		"memory_search", "memory_recent", "dream_recap", "fetch_url", "web_search",
		"list_files", "glob", "grep", "list_providers", "schedule_list",
		"debug_tools", "debug_storage", "debug_policy",
	}

	t.Run("registered tools are named", func(t *testing.T) {
		t.Parallel()
		got := humanisationRule([]ToolInfo{{Name: "glob"}, {Name: "web_search"}})
		for _, want := range []string{"glob", "web_search"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s is registered but not named", want)
			}
		}
	})

	t.Run("unregistered tools are never named", func(t *testing.T) {
		t.Parallel()
		got := humanisationRule([]ToolInfo{{Name: "glob"}})
		for _, name := range mentionable {
			if name == "glob" {
				continue
			}
			if strings.Contains(got, name) {
				t.Errorf("%s is not registered but the rule names it", name)
			}
		}
	})

	t.Run("guidance survives losing every example", func(t *testing.T) {
		t.Parallel()
		got := humanisationRule(nil)
		// The shapes are the point; the tools only illustrate them.
		for _, want := range []string{"Narrative content", "Fact-dense", "own register", "markdown"} {
			if !strings.Contains(got, want) {
				t.Errorf("dropping the examples took %q with it", want)
			}
		}
		// An empty parenthetical is the failure to avoid.
		if strings.Contains(got, "()") || strings.Contains(got, "( )") {
			t.Errorf("an empty example list was emitted:\n%s", got)
		}
	})

	t.Run("the introspection bullet goes with its family", func(t *testing.T) {
		t.Parallel()
		// This bullet is ABOUT debug_*, not merely illustrated by it,
		// so with the family disabled it describes how to render
		// output that can no longer be produced.
		without := humanisationRule([]ToolInfo{{Name: "glob"}})
		if strings.Contains(without, "Operator introspection") {
			t.Error("the introspection bullet survived its whole family being disabled")
		}
		with := humanisationRule([]ToolInfo{{Name: "debug_storage"}})
		if !strings.Contains(with, "Operator introspection") {
			t.Error("the introspection bullet is missing though debug_storage is registered")
		}
	})
}
