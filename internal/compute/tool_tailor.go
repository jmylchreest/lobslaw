package compute

import (
	"regexp"
	"strings"
)

// Constants that drive the classifier (toolTailorDefaults,
// toolCategoryPatterns) live in tool_tailor_keywords.go so
// keyword tuning doesn't require reading the dispatch code.

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
