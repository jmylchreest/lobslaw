package gateway

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
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
		inv  compute.ToolInvocation
		want string
	}{
		{
			"path arg",
			compute.ToolInvocation{ToolName: "read_file", Args: `{"path":"/etc/hosts"}`},
			"ReadFile(/etc/hosts)",
		},
		{
			"pattern arg",
			compute.ToolInvocation{ToolName: "grep", Args: `{"pattern":"lobslaw","path":"/x"}`},
			"Grep(lobslaw)",
		},
		{
			"url arg",
			compute.ToolInvocation{ToolName: "fetch_url", Args: `{"url":"https://example.com"}`},
			"FetchUrl(https://example.com)",
		},
		{
			"no args",
			compute.ToolInvocation{ToolName: "current_time", Args: `{}`},
			"CurrentTime()",
		},
		{
			"namespaced mcp",
			compute.ToolInvocation{ToolName: "gmail.search", Args: `{"query":"urgent"}`},
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
	inv := compute.ToolInvocation{
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
	if deniedByPolicy(compute.ToolInvocation{Error: "some other error"}) != "" {
		t.Error("should only match policy-denied errors")
	}
	if deniedByPolicy(compute.ToolInvocation{Error: ""}) != "" {
		t.Error("empty error should yield empty reason")
	}
}

func TestToolCallBreadcrumbFiltersFailures(t *testing.T) {
	t.Parallel()
	calls := []compute.ToolInvocation{
		{ToolName: "grep", Args: `{"pattern":"foo"}`},
		{ToolName: "read_file", Args: `{"path":"/x"}`, Error: "some error"},
	}
	got := toolCallBreadcrumb(calls)
	if !strings.Contains(got, "Grep(foo)") {
		t.Errorf("breadcrumb missing successful call: %q", got)
	}
	if strings.Contains(got, "ReadFile(/x)") {
		t.Errorf("breadcrumb should omit failed calls: %q", got)
	}
}

func TestToolCallBreadcrumbCompactsManyCalls(t *testing.T) {
	t.Parallel()
	// 8 fetch_url + 2 read_file → over the expanded limit, so
	// breadcrumb should show counts not individual entries.
	var calls []compute.ToolInvocation
	for range 8 {
		calls = append(calls, compute.ToolInvocation{ToolName: "fetch_url", Args: `{"url":"https://x"}`})
	}
	for range 2 {
		calls = append(calls, compute.ToolInvocation{ToolName: "read_file", Args: `{"path":"/y"}`})
	}
	got := toolCallBreadcrumb(calls)
	if !strings.Contains(got, "8× `FetchUrl`") {
		t.Errorf("breadcrumb missing FetchUrl count: %q", got)
	}
	if !strings.Contains(got, "2× `ReadFile`") {
		t.Errorf("breadcrumb missing ReadFile count: %q", got)
	}
	if strings.Contains(got, "https://x") {
		t.Errorf("compact form should not list individual args: %q", got)
	}
}

func TestToolCallBreadcrumbExpandsForSmallSets(t *testing.T) {
	t.Parallel()
	calls := []compute.ToolInvocation{
		{ToolName: "grep", Args: `{"pattern":"foo"}`},
		{ToolName: "read_file", Args: `{"path":"/x"}`},
	}
	got := toolCallBreadcrumb(calls)
	if !strings.Contains(got, "Grep(foo)") {
		t.Errorf("small set should expand: %q", got)
	}
	if strings.Contains(got, "×") {
		t.Errorf("small set should NOT use count form: %q", got)
	}
}

func TestToolCallBreadcrumbEmpty(t *testing.T) {
	t.Parallel()
	if toolCallBreadcrumb(nil) != "" {
		t.Error("nil calls should yield empty breadcrumb")
	}
	if toolCallBreadcrumb([]compute.ToolInvocation{{Error: "fail"}}) != "" {
		t.Error("all-failed calls should yield empty breadcrumb")
	}
}
