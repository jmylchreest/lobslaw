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
//
// debug_tools / debug_memory / debug_storage are included so the
// bot can ALWAYS self-introspect when asked "what can you do" or
// "is memory empty". Without these as defaults, the bot looked
// at its (tailored) function-calling schema, saw 3-4 tools, and
// confidently reported those as its full capability — falsely
// "I only have memory_search and current_time" when 30 tools
// were registered. Self-introspection is not optional.
var toolTailorDefaults = map[string]bool{
	"current_time":    true,
	"memory_search":   true,
	"memory_write":    true,
	"memory_recent":   true,
	"debug_tools":     true,
	"debug_memory":    true,
	"debug_storage":   true,
	"debug_providers": true,
	// notify_telegram is the proactive-push primitive; always
	// admitted because the firing-turn user message of a
	// commitment / scheduled task usually doesn't match any
	// keyword (it's the bot's own stored prompt). Without this,
	// fired commitments generate text that goes nowhere.
	"notify_telegram": true,
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
	// ---- Scheduler (recurring) ------------------------------
	// Cron-style recurring tasks: 'every 5m check mail',
	// 'every morning at 8am'. For one-shot 'in 2 minutes', use
	// the commitment category below instead.
	{
		category: "scheduler",
		tools:    []string{"schedule_create", "schedule_list", "schedule_get", "schedule_delete"},
		keywords: []string{
			"every 5", "every 10", "every 15", "every 30", "every hour",
			"every morning", "every evening", "daily at",
			"check every", "remind me every", "poll every",
			"scheduled task", "scheduled tasks", "my schedules",
			"cancel schedule", "stop schedule", "remove schedule",
			"list schedule", "what schedules", "running tasks",
		},
	},
	// ---- Commitments (one-shot) -----------------------------
	// One-shot future actions: 'in 2 minutes', 'in an hour',
	// 'tomorrow at 9am', 'later', 'when I'm done'. notify_telegram
	// is admitted alongside because most commitments need to
	// deliver their result back to the user proactively (the
	// firing turn has no chat to reply into automatically).
	{
		category: "commitment",
		tools:    []string{"commitment_create", "commitment_list", "commitment_cancel", "notify_telegram"},
		keywords: []string{
			"in 1 minute", "in 2 minute", "in 5 minute", "in 10 minute",
			"in 15 minute", "in 30 minute", "in 1 hour", "in 2 hour",
			"in an hour", "in half an hour", "in a minute",
			"remind me to ", "remind me in ", "remind me at ",
			"remind me tomorrow", "ping me when", "ping me in",
			"message me in", "message me when", "tell me in",
			"in a bit", "later today", "tonight", "tomorrow morning",
			"my commitments", "my reminders", "what reminders",
			"cancel reminder", "cancel commitment",
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
