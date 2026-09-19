//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/computer"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == sandbox.HelperSubcommand {
		policy, err := sandbox.DecodePolicy(os.Getenv(sandbox.PolicyEnvVar))
		if err == nil {
			_ = os.Unsetenv(sandbox.PolicyEnvVar)
			err = sandbox.InstallAndExec(policy, os.Args[3], os.Args[3:], os.Environ())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}

type browserOwnerAuthorizer struct{}

func (browserOwnerAuthorizer) AuthorizeProject(_ context.Context, owner, project string) error {
	if owner != "user:alice" || project != "project" {
		return computer.ErrForbidden
	}
	return nil
}

func TestRealBrowserThroughBotExecutor(t *testing.T) {
	node := os.Getenv("COMPUTER_TEST_NODE")
	if node == "" {
		t.Skip("set COMPUTER_TEST_NODE/CHROMIUM/PLAYWRIGHT for real bot-browser QA")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/login" {
			_, _ = fmt.Fprint(w, `<form><input id="password" type="password" value="fixture-password-secret"><input name="username" value="fixture-login-secret"></form>`)
			return
		}
		_, _ = fmt.Fprint(w, `<h1>Search workspace</h1><input id="query" type="search"><button id="search" onclick="document.querySelector('#result').textContent='Found '+document.querySelector('#query').value">Search</button><p id="result"></p>`)
	}))
	defer fixture.Close()
	root := t.TempDir()
	socket := filepath.Join(t.TempDir(), "egress.sock")
	proxy, err := egress.NewSmokescreenProvider(egress.SmokescreenConfig{UDSPath: socket, ACL: egress.Rules{Roles: map[string][]string{"computer": {"127.0.0.1"}}}, AllowRanges: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Stop(context.Background()) }()
	cfg := computer.Config{Root: filepath.Join(root, "private"), Node: node, Chromium: os.Getenv("COMPUTER_TEST_CHROMIUM"), Playwright: os.Getenv("COMPUTER_TEST_PLAYWRIGHT"), IP: "/usr/sbin/ip", ProxySocket: socket,
		ProxyAddress: proxy.ProxyURL().Host,
		ReadPaths:    []string{"/usr", "/lib", "/lib64", "/etc/fonts", "/etc/ssl", "/etc/ld.so.cache", "/proc", "/sys", filepath.Dir(node), filepath.Dir(os.Getenv("COMPUTER_TEST_CHROMIUM")), filepath.Dir(os.Getenv("COMPUTER_TEST_PLAYWRIGHT"))}}
	service := computer.New(cfg, browserOwnerAuthorizer{})
	defer func() { _ = service.Close() }()
	call := func(id, name string, args map[string]string) compute.MockResponse {
		raw, _ := json.Marshal(args)
		return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: id, Name: name, Arguments: string(raw)}}}
	}
	agent, _ := browserAgent(t, service, []string{"browser_navigate", "browser_fill", "browser_click", "browser_capture"},
		call("nav", "browser_navigate", map[string]string{"url": fixture.URL}),
		call("query", "browser_fill", map[string]string{"selector": "#query", "value": "red pandas"}),
		call("search", "browser_click", map[string]string{"selector": "#search"}),
		call("observe", "browser_capture", map[string]string{}),
		call("login", "browser_navigate", map[string]string{"url": fixture.URL + "/login"}),
		call("protected", "browser_fill", map[string]string{"selector": "#password", "value": "refused test input"}),
		compute.MockResponse{Content: "Search complete; owner login required."})
	req := turn.Request{Message: "Search the project site", Claims: &types.Claims{UserID: "alice"}, Principal: identity.Bot("worker"), BotID: "worker", Channel: "workforce", ChannelID: "project"}
	response, err := agent.Run(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.ToolCalls) != 6 {
		t.Fatalf("browser tools did not run: %+v", response.ToolCalls)
	}
	raw, _ := json.Marshal(response.Messages)
	if !strings.Contains(string(raw), "Found red pandas") || !strings.Contains(string(raw), "nth-of-type") {
		t.Fatalf("bot lacked actionable browser observation: %s", raw)
	}
	if strings.Contains(string(raw), "fixture-password-secret") || strings.Contains(string(raw), "fixture-login-secret") || strings.Contains(string(raw), "screenshot") {
		t.Fatal("credentials or screenshot payload leaked into bot transcript")
	}
	if !strings.Contains(string(raw), "sensitive step requires human") {
		t.Fatal("credential fill was not refused")
	}
	// A reviewed literal independently replays a routine's harmless search step.
	for _, step := range []computer.RoutineStep{{Action: "navigate", URL: fixture.URL}, {Action: "fill", Selector: "#query", Value: "routine search", InputMode: computer.InputReviewedLiteral}, {Action: "click", Selector: "#search"}} {
		if err := service.ExecuteStep(t.Context(), "user:alice", "project", step); err != nil {
			t.Fatal(err)
		}
	}
	result, err := service.RunStep(t.Context(), "user:alice", "project", computer.RoutineStep{Action: "capture"})
	if err != nil || result.Observation == nil || !strings.Contains(result.Observation.Text, "Found routine search") {
		t.Fatalf("reviewed fill did not replay: %v %+v", err, result.Observation)
	}
	if _, err := service.Action(t.Context(), "user:alice", "project", computer.RoutineStep{Action: "takeover"}); err != nil {
		t.Fatal(err)
	}
	blocked, _ := browserAgent(t, service, []string{"browser_capture"}, call("blocked", "browser_capture", map[string]string{}), compute.MockResponse{Content: "Waiting for owner"})
	response, err = blocked.Run(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(response.Messages)
	if !strings.Contains(string(raw), "human has control") {
		t.Fatal("bot bypassed takeover")
	}
	if _, err := service.RunStep(t.Context(), "user:bob", "project", computer.RoutineStep{Action: "capture"}); !errors.Is(err, computer.ErrForbidden) {
		t.Fatal("cross-owner bot observed browser")
	}
}
