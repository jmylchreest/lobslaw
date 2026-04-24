package compute

import "regexp"

// This file holds ONLY the data lists the tool-tailor classifier
// matches against. Kept in a separate file from tool_tailor.go so
// operators + contributors can tune keyword lists without reading
// the dispatch logic. Each category admits every tool in its
// tools list whenever ANY keyword/regex hits the lowercased user
// message.
//
// When adding a category:
//  1. Append a toolCategory entry below
//  2. Ensure each keyword is space-padded ("weather " not
//     "weather") where false-positive risk is high (partial word
//     matches — e.g. "weather" would match "weathering" which is
//     fine here but worth noting for tighter domains).
//  3. Use regexes only when a keyword approach would miss
//     structured forms (URLs, phone numbers, dates).

// toolTailorDefaults are tools that stay in every turn's list
// regardless of classifier output. Cheap to advertise, broadly
// applicable. Removing entries here is almost always wrong.
var toolTailorDefaults = map[string]bool{
	"current_time":  true,
	"memory_search": true,
	"memory_write":  true,
}

// toolCategoryPatterns groups tools by intent category.
var toolCategoryPatterns = []toolCategory{
	// ---- Filesystem read ------------------------------------
	{
		category: "filesystem_read",
		tools:    []string{"read_file", "grep", "list_files", "glob"},
		keywords: []string{
			"file ", "read ", "open ", "show me", "cat ", "look at",
			"search for", "grep", "find in", "contents of",
			".go", ".md", ".py", ".ts", ".json", ".yaml", ".toml",
			"directory", "folder",
			"/home/", "/etc/", "/var/", "/tmp/",
		},
	},
	// ---- Filesystem write -----------------------------------
	{
		category: "filesystem_write",
		tools:    []string{"write_file", "edit_file"},
		keywords: []string{
			"write ", "create ", "save ", "edit ", "change ", "update ",
			"modify ", "replace ", "add to", "append ", "overwrite",
		},
	},
	// ---- Shell ----------------------------------------------
	{
		category: "shell",
		tools:    []string{"shell_command"},
		keywords: []string{
			"run ", "execute ", "shell", "bash", "command",
			"ls ", "ps ", "kill ", "git ", "process", "install ",
		},
	},
	// ---- Web ------------------------------------------------
	// Broad on purpose: weather / forecast / prices / sports
	// scores are all "public website can answer this" and got
	// refused as "I don't have a tool" before we admitted
	// these patterns.
	{
		category: "web",
		tools:    []string{"fetch_url", "web_search"},
		keywords: []string{
			"web ", "internet", "online", "google ", "look up",
			"latest news", "latest version", "latest release",
			"news about", "recent news", "current events",
			"wikipedia", " article ",
			// Code hosting / online references
			"github", "gitlab", "bitbucket", "the repo", "repository",
			"pull request", "issue tracker", "readme online",
			"website", "webpage", "homepage", "documentation online",
			"blog post", "fetch the", "fetch this",
			// Weather / environment
			"weather", "forecast", "temperature", "rain",
			"humidity", "wind", "snow", "sunrise", "sunset",
			// Market / finance
			"price", "stock price", "share price", "exchange rate",
			// Sports / events
			"score", "match result", "fixture", "standings",
		},
		regexes: []*regexp.Regexp{
			regexp.MustCompile(`https?://`),
		},
	},
	// ---- Provider council -----------------------------------
	// Explicit user requests for multi-provider review. Keywords
	// must be specific — "council" shouldn't fire on "I had a
	// council meeting yesterday", so we require the verification
	// verbs nearby ("review", "second opinion", etc.).
	{
		category: "council",
		tools:    []string{"list_providers", "council_review"},
		keywords: []string{
			"second opinion", "adversarial review", "council review",
			"the council", "get the council", "verify this", "cross-check",
			"ask the council", "consensus check", "fan out",
			"which providers", "what providers", "list providers",
		},
	},
	// ---- Memory introspection -------------------------------
	// Distinct from memory_search (the always-on default): these
	// list-style queries want the newest N memories, not a keyword
	// match. Triggered on "recently", "lately", "learned about me".
	{
		category: "memory_introspection",
		tools:    []string{"memory_recent"},
		keywords: []string{
			"recently", "lately", "this week", "last few days",
			"what have you learned", "what do you remember",
			"recent memories", "latest memories", "what's new",
			"new memories", "recent learnings",
		},
	},
	// ---- Debug / introspection ------------------------------
	{
		category: "debug",
		tools: []string{
			"debug_tools", "debug_policy", "debug_storage",
			"debug_memory", "debug_soul", "debug_raft",
			"debug_scheduler", "debug_providers", "debug_version",
		},
		keywords: []string{
			"debug", "diagnose", "introspect",
			"what tools", "which tools", "tool list",
			"policy rule", "policy rules",
			"storage mount", "storage mounts",
			"raft leader", "raft state",
			"scheduled task", "scheduled tasks",
			"providers",
		},
	},
}

// toolCategory is the shape each entry in toolCategoryPatterns
// takes. Extracted as a named type so the list above reads as
// data rather than anonymous struct noise.
type toolCategory struct {
	category string
	tools    []string
	keywords []string
	regexes  []*regexp.Regexp
}
