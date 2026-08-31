package promptgen

import "strings"

// humanisationRule tells the model how to re-render tool output.
//
// The examples name real tools, so they are filtered against the turn's
// registry for the same reason the operating principles are: a rule
// illustrated with tools the model does not have teaches the shape by
// pointing at nothing.
//
// Weaker than the safety case, deliberately. This is a rendering guide
// — output shaped like this renders like that — so a stale name here
// misleads rather than instructs, and the guidance survives losing its
// examples. Only the debug_* bullet goes entirely when its family
// does, because that bullet is ABOUT the family rather than
// illustrated by it.
func humanisationRule(tools []ToolInfo) string {
	have := make(map[string]bool, len(tools))
	for _, t := range tools {
		have[t.Name] = true
	}
	named := func(names ...string) string {
		live := make([]string, 0, len(names))
		for _, n := range names {
			if have[n] {
				live = append(live, n)
			}
		}
		return strings.Join(live, ", ")
	}
	egs := func(list string) string {
		if list == "" {
			return ""
		}
		return " (" + list + ")"
	}

	var b strings.Builder
	b.WriteString("Tools return structured JSON. Always re-render that output for the user, picking the format that fits the content type:\n\n")

	b.WriteString("- **Narrative content**" +
		egs(named("memory_search", "memory_recent", "dream_recap", "fetch_url", "web_search")) +
		": speak in your own register. Talk about what you learned, in your own words, rather than reciting fields.\n")

	b.WriteString("- **Fact-dense / enumerable content**" +
		egs(named("list_files", "glob", "grep", "list_providers", "schedule_list")) +
		": render as a markdown bullet list or table. A list of 20 files belongs in a table with name/size/modified columns.\n")

	// Named per-tool rather than as the debug_* glob, so the bullet
	// disappears when the family is disabled instead of describing how
	// to render output that can no longer be produced.
	if dbg := named("debug_tools", "debug_policy", "debug_storage", "debug_memory",
		"debug_soul", "debug_raft", "debug_scheduler", "debug_providers",
		"debug_version", "debug_sandbox", "debug_mcp"); dbg != "" {
		b.WriteString("- **Operator introspection output** (" + dbg +
			"): render verbatim or as a clean markdown table. These want exact values, so quote them as-is — someone asking what a mount path is wants the path itself.\n")
	}
	return b.String()
}
