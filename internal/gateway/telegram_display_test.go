package gateway

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

func TestPrettyToolName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":              "Tool",
		"grep":          "Grep",
		"read_file":     "ReadFile",
		"list_files":    "ListFiles",
		"memory_search": "MemorySearch",
		"gmail.search":  "Gmail.Search",
		"mcp_add":       "McpAdd",
		"debug_sandbox": "DebugSandbox",
	}
	for in, want := range cases {
		if got := prettyToolName(in); got != want {
			t.Errorf("prettyToolName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestFormatToolCallPrimaryArg(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		inv  turn.ToolInvocation
		want string
	}{
		{
			"path arg",
			turn.ToolInvocation{ToolName: "read_file", Args: `{"path":"/etc/hosts"}`},
			"ReadFile(/etc/hosts)",
		},
		{
			"pattern arg",
			turn.ToolInvocation{ToolName: "grep", Args: `{"pattern":"lobslaw","path":"/x"}`},
			"Grep(lobslaw)",
		},
		{
			"url arg",
			turn.ToolInvocation{ToolName: "fetch_url", Args: `{"url":"https://example.com"}`},
			"FetchUrl(https://example.com)",
		},
		{
			"no args",
			turn.ToolInvocation{ToolName: "current_time", Args: `{}`},
			"CurrentTime()",
		},
		{
			"namespaced mcp",
			turn.ToolInvocation{ToolName: "gmail.search", Args: `{"query":"urgent"}`},
			"Gmail.Search(urgent)",
		},
	}
	for _, tc := range cases {
		if got := formatToolCall(tc.inv); got != tc.want {
			t.Errorf("%s: formatToolCall = %q; want %q", tc.name, got, tc.want)
		}
	}
}

func TestDeniedByPolicyExtractsReason(t *testing.T) {
	t.Parallel()
	inv := turn.ToolInvocation{
		Error: "policy denied: subject=public; action=shell_command; rule=deny-all-shell",
	}
	got := deniedByPolicy(inv)
	if !strings.Contains(got, "subject=public") {
		t.Errorf("reason extraction: %q", got)
	}
	if got == "" {
		t.Error("empty reason when policy denied substring present")
	}
}

func TestDeniedByPolicyNoMatch(t *testing.T) {
	t.Parallel()
	if deniedByPolicy(turn.ToolInvocation{Error: "some other error"}) != "" {
		t.Error("should only match policy-denied errors")
	}
	if deniedByPolicy(turn.ToolInvocation{Error: ""}) != "" {
		t.Error("empty error should yield empty reason")
	}
}
