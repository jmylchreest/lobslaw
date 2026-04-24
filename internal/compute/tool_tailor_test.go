package compute

import (
	"testing"
)

func allAvailableTools() []Tool {
	return []Tool{
		{Name: "current_time"},
		{Name: "memory_search"},
		{Name: "memory_write"},
		{Name: "read_file"},
		{Name: "search_files"},
		{Name: "write_file"},
		{Name: "edit_file"},
		{Name: "shell_command"},
		{Name: "fetch_url"},
		{Name: "web_search"},
	}
}

func toolNameSet(tools []Tool) map[string]bool {
	out := map[string]bool{}
	for _, t := range tools {
		out[t.Name] = true
	}
	return out
}

func TestTailoredToolsKeepsDefaults(t *testing.T) {
	t.Parallel()
	out := tailoredToolsFor("just say hello", allAvailableTools())
	set := toolNameSet(out)
	for _, name := range []string{"current_time", "memory_search", "memory_write"} {
		if !set[name] {
			t.Errorf("default tool %q missing from tailored list", name)
		}
	}
}

func TestTailoredToolsUnlocksFilesystemRead(t *testing.T) {
	t.Parallel()
	out := tailoredToolsFor("read the file at /home/x/notes.md", allAvailableTools())
	set := toolNameSet(out)
	if !set["read_file"] {
		t.Error("read_file should be admitted for a file-read intent")
	}
	if !set["search_files"] {
		t.Error("search_files should be admitted alongside read_file")
	}
	if set["shell_command"] {
		t.Error("shell_command should NOT be admitted for simple read intent")
	}
}

func TestTailoredToolsUnlocksWebForURLs(t *testing.T) {
	t.Parallel()
	out := tailoredToolsFor("summarise https://example.com/article for me", allAvailableTools())
	set := toolNameSet(out)
	if !set["fetch_url"] {
		t.Error("fetch_url should be admitted when message contains a URL")
	}
	if !set["web_search"] {
		t.Error("web_search should be admitted for web category")
	}
}

func TestTailoredToolsUnlocksWebForKeyword(t *testing.T) {
	t.Parallel()
	out := tailoredToolsFor("what's the latest news about AI", allAvailableTools())
	set := toolNameSet(out)
	if !set["web_search"] {
		t.Error("web_search should be admitted for news/latest queries")
	}
}

func TestTailoredToolsUnlocksShell(t *testing.T) {
	t.Parallel()
	out := tailoredToolsFor("run git status for me", allAvailableTools())
	set := toolNameSet(out)
	if !set["shell_command"] {
		t.Error("shell_command should be admitted for shell intents")
	}
}

func TestTailoredToolsFiltersUnrelated(t *testing.T) {
	t.Parallel()
	out := tailoredToolsFor("how are you feeling today?", allAvailableTools())
	set := toolNameSet(out)
	for _, name := range []string{"shell_command", "write_file", "fetch_url"} {
		if set[name] {
			t.Errorf("%q should NOT be admitted for a conversational turn", name)
		}
	}
}

func TestTailoredToolsFallsBackWhenDefaultsMissing(t *testing.T) {
	t.Parallel()
	// Registry without current_time — tailor should NOT strip to
	// empty; better to keep the full (small) list than hand the
	// model nothing.
	available := []Tool{{Name: "read_file"}, {Name: "shell_command"}}
	out := tailoredToolsFor("hi", available)
	if len(out) != len(available) {
		t.Errorf("want full fallback list; got %d entries", len(out))
	}
}
