package gateway

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// formatToolCall renders a single ToolInvocation opencode-style:
//
//	Read(/etc/hosts)
//	Grep("lobslaw", /home/johnm)
//	Gmail.Search(query="urgent")
//
// The name is title-cased per segment (so "gmail.search" →
// "Gmail.Search" and "read_file" → "ReadFile"). The primary
// argument appears raw in parens when there's one obvious "main"
// field (path, pattern, url, query); otherwise a compact
// key=value list.
func formatToolCall(inv compute.ToolInvocation) string {
	display := prettyToolName(inv.ToolName)
	arg := primaryArgDisplay(inv.Args)
	if arg == "" {
		return fmt.Sprintf("%s()", display)
	}
	return fmt.Sprintf("%s(%s)", display, arg)
}

// prettyToolName converts lowercase_underscore.dotted into
// TitleCase.Dotted. "grep" → "Grep", "gmail.search" →
// "Gmail.Search", "read_file" → "ReadFile",
// "memory_search" → "MemorySearch".
func prettyToolName(name string) string {
	if name == "" {
		return "Tool"
	}
	segments := strings.Split(name, ".")
	for i, seg := range segments {
		parts := strings.Split(seg, "_")
		for j, p := range parts {
			if p == "" {
				continue
			}
			r := []rune(p)
			r[0] = unicode.ToUpper(r[0])
			parts[j] = string(r)
		}
		segments[i] = strings.Join(parts, "")
	}
	return strings.Join(segments, ".")
}

// primaryArgDisplay picks a representative argument from the tool
// call's JSON-encoded arguments. Known "headline" fields come out
// raw (Read(/etc/hosts)); anything else renders compactly as
// key=value pairs joined with commas.
//
// Empty arg string → "" so the caller renders Tool() without parens
// contents.
func primaryArgDisplay(rawJSON string) string {
	rawJSON = strings.TrimSpace(rawJSON)
	if rawJSON == "" || rawJSON == "{}" {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(rawJSON), &args); err != nil {
		if len(rawJSON) > 60 {
			rawJSON = rawJSON[:57] + "..."
		}
		return rawJSON
	}
	if len(args) == 0 {
		return ""
	}
	// Order matters: for a tool taking both a pattern and a path
	// (grep), the semantic "what" is the pattern — we want
	// Grep(lobslaw) not Grep(/home/johnm). For read_file, path is
	// the only primary arg, so it falls through to later in the
	// list without being shadowed.
	for _, field := range []string{"pattern", "query", "url", "path", "id", "name"} {
		if v, ok := args[field]; ok {
			return compactValue(v)
		}
	}
	// Compact key=value list for tools without an obvious primary.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, compactValue(args[k])))
	}
	joined := strings.Join(parts, ", ")
	if len(joined) > 80 {
		joined = joined[:77] + "..."
	}
	return joined
}

// toolCallBreadcrumb renders a compact "ran X, Y, Z" line
// summarising what tools the turn invoked. Used as an italic
// prefix on the final reply when the SOUL is chatty enough that
// tool-call transparency is wanted. Direct-SOUL deployments skip
// this — terse replies shouldn't carry breadcrumbs. Returns empty
// string when there are no successful tool invocations to
// summarise.
func toolCallBreadcrumb(calls []compute.ToolInvocation) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, inv := range calls {
		if inv.Error != "" {
			continue
		}
		parts = append(parts, "`"+formatToolCall(inv)+"`")
	}
	if len(parts) == 0 {
		return ""
	}
	return "_ran: " + strings.Join(parts, ", ") + "_"
}

// notifyPolicyDenials emits one Telegram message per tool call
// that was denied by policy. Kept as a separate interstitial
// (rather than folded into the final reply) so the user always
// sees policy enforcement, regardless of whether the LLM chose
// to narrate the failure in its final text.
func (h *TelegramHandler) notifyPolicyDenials(chatID int64, calls []compute.ToolInvocation) {
	for _, inv := range calls {
		if reason := deniedByPolicy(inv); reason != "" {
			display := formatToolCall(inv)
			msg := fmt.Sprintf("🚫 *Policy denied* `%s`", display)
			if reason != "" {
				msg += "\nReason: " + reason
			}
			h.sendText(chatID, msg)
		}
	}
}

func compactValue(v any) string {
	s := fmt.Sprint(v)
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// deniedByPolicy returns the deny reason when the invocation error
// looks like a policy denial, empty string otherwise. The Executor
// wraps these with "policy denied: <reason>"; the pattern survives
// the stringification in inv.Error.
func deniedByPolicy(inv compute.ToolInvocation) string {
	if inv.Error == "" {
		return ""
	}
	if idx := strings.Index(inv.Error, "policy denied"); idx >= 0 {
		rest := inv.Error[idx+len("policy denied"):]
		rest = strings.TrimPrefix(rest, ":")
		rest = strings.TrimSpace(rest)
		return rest
	}
	return ""
}
