package compute

import (
	"regexp"
	"strings"
)

// toolTailorDefaults are tools that stay in every turn's advertised
// list regardless of classifier output. current_time + memory_*
// are cheap to advertise (small schemas), broadly applicable, and
// the model loses utility without them across essentially any
// turn shape.
var toolTailorDefaults = map[string]bool{
	"current_time":   true,
	"memory_search":  true,
	"memory_write":   true,
}

// toolCategoryPatterns groups tools by the kind of intent that
// should unlock them. Keywords are lowercased substrings tested
// against the user message. Hits on ANY keyword in a category
// admit EVERY tool in that category for this turn.
var toolCategoryPatterns = []struct {
	category string
	tools    []string
	keywords []string
	regexes  []*regexp.Regexp
}{
	{
		category: "filesystem_read",
		tools:    []string{"read_file", "search_files"},
		keywords: []string{
			"file ", "read ", "open ", "show me", "cat ", "look at",
			"search for", "grep", "find in", "contents of", ".go", ".md",
			".py", ".ts", ".json", ".yaml", ".toml", "directory", "folder",
			"/home/", "/etc/", "/var/", "/tmp/",
		},
	},
	{
		category: "filesystem_write",
		tools:    []string{"write_file", "edit_file"},
		keywords: []string{
			"write ", "create ", "save ", "edit ", "change ", "update ",
			"modify ", "replace ", "add to", "append ", "overwrite",
		},
	},
	{
		category: "shell",
		tools:    []string{"shell_command"},
		keywords: []string{
			"run ", "execute ", "shell", "bash", "command", "ls ", "ps ",
			"kill ", "git ", "process", "install ",
		},
	},
	{
		category: "web",
		tools:    []string{"fetch_url", "web_search"},
		keywords: []string{
			"web ", "internet", "online", "google ", "look up",
			"latest news", "latest version", "latest release",
			"news about", "recent news", "current events",
			"wikipedia", " article ",
		},
		regexes: []*regexp.Regexp{
			regexp.MustCompile(`https?://`),
		},
	},
}

// tailoredToolsFor returns the subset of available that the
// heuristic thinks the model will actually need for userMessage.
// Defaults (current_time, memory_*) always pass through. A
// category is admitted when any of its keywords or regexes hit
// the lowercased message.
//
// Non-goal: perfect recall. The model still has the explicit
// memory_search tool for anything we failed to anticipate, and
// the full registry continues to exist — we're just trimming the
// per-turn advertisement.
func tailoredToolsFor(userMessage string, available []Tool) []Tool {
	lower := strings.ToLower(userMessage)
	allowed := map[string]bool{}
	for name := range toolTailorDefaults {
		allowed[name] = true
	}
	for _, cat := range toolCategoryPatterns {
		if categoryHits(lower, cat.keywords, cat.regexes) {
			for _, t := range cat.tools {
				allowed[t] = true
			}
		}
	}
	out := make([]Tool, 0, len(available))
	for _, t := range available {
		if allowed[t.Name] {
			out = append(out, t)
		}
	}
	// Safety net: if the heuristic ended up returning fewer than
	// the default set (e.g. the registry doesn't actually have
	// current_time), fall back to advertising everything. Better
	// to bloat the prompt than hand the model zero tools.
	if len(out) < len(toolTailorDefaults) {
		return available
	}
	return out
}

func categoryHits(lower string, keywords []string, regexes []*regexp.Regexp) bool {
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	for _, re := range regexes {
		if re.MatchString(lower) {
			return true
		}
	}
	return false
}
