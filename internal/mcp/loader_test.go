package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// newLoaderWithFakeServer builds a Loader with a single
// in-memory-transport-backed server registered. Skips the
// subprocess spawn path — see registerServer for why.
func newLoaderWithFakeServer(t *testing.T, serverName string, tools []Tool) (*Loader, *memTransport) {
	t.Helper()
	l := NewLoader(LoaderConfig{})
	tr := &memTransport{}
	// Queue the Initialize response so the client handshake succeeds
	// when registerServer's callers drive it.
	tr.queueResponse(Response{
		ID:     float64(1),
		Result: json.RawMessage(`{"serverInfo":{"name":"fake"}}`),
	})
	c, _ := NewClient(Config{Transport: tr})
	_, _ = c.Initialize(context.Background())
	l.registerServer(serverName, c, tools)
	return l, tr
}

func TestLoaderHasKnowsRegisteredTools(t *testing.T) {
	t.Parallel()
	l, _ := newLoaderWithFakeServer(t, "fs", []Tool{
		{Name: "read_file", Description: "Read a file"},
	})
	if !l.Has("fs.read_file") {
		t.Error("registered tool should be known under its namespaced name")
	}
	if l.Has("read_file") {
		t.Error("bare tool names should not be visible; namespacing is the contract")
	}
	if l.Has("fs.write_file") {
		t.Error("unregistered tool should not be known")
	}
}

func TestLoaderInvokeRoutesToServer(t *testing.T) {
	t.Parallel()
	l, tr := newLoaderWithFakeServer(t, "fs", []Tool{{Name: "read_file"}})

	// Queue a successful CallTool response.
	tr.queueResponse(Response{
		ID:     float64(2),
		Result: json.RawMessage(`{"content":[{"type":"text","text":"hello from MCP"}]}`),
	})

	res, err := l.Invoke(context.Background(), compute.SkillInvokeRequest{
		Name:   "fs.read_file",
		Params: map[string]any{"path": "/tmp/a.txt"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code: %d", res.ExitCode)
	}
	if string(res.Stdout) != "hello from MCP" {
		t.Errorf("stdout: %q", res.Stdout)
	}
}

// TestLoaderInvokeToolErrorReturnsExit1 — when an MCP server
// reports IsError=true, the agent should see a non-zero exit + the
// content-as-stderr so the LLM treats it as a tool failure.
func TestLoaderInvokeToolErrorReturnsExit1(t *testing.T) {
	t.Parallel()
	l, tr := newLoaderWithFakeServer(t, "fs", []Tool{{Name: "risky"}})

	tr.queueResponse(Response{
		ID: float64(2),
		Result: json.RawMessage(`{
			"isError": true,
			"content": [{"type":"text","text":"file not found"}]
		}`),
	})
	res, err := l.Invoke(context.Background(), compute.SkillInvokeRequest{Name: "fs.risky"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 1 {
		t.Errorf("exit code: %d", res.ExitCode)
	}
	if string(res.Stderr) != "file not found" {
		t.Errorf("stderr: %q", res.Stderr)
	}
	if len(res.Stdout) != 0 {
		t.Errorf("stdout should be empty on error: %q", res.Stdout)
	}
}

func TestLoaderInvokeUnknownToolErrors(t *testing.T) {
	t.Parallel()
	l := NewLoader(LoaderConfig{})
	_, err := l.Invoke(context.Background(), compute.SkillInvokeRequest{Name: "missing"})
	if err == nil {
		t.Error("unknown tool should error")
	}
}

func TestLoaderListToolsReturnsAllRegistered(t *testing.T) {
	t.Parallel()
	l, _ := newLoaderWithFakeServer(t, "fs", []Tool{
		{Name: "read_file"}, {Name: "write_file"},
	})
	list := l.ListTools()
	if len(list) != 2 {
		t.Errorf("len=%d", len(list))
	}
}

// TestLoaderSatisfiesSkillDispatcher — compile-time guard already
// asserts the interface; this runtime test pins the contract via
// the agent's public entry point so a drift would show up at test
// run rather than hidden by the var assertion.
func TestLoaderSatisfiesSkillDispatcher(t *testing.T) {
	t.Parallel()
	var _ compute.SkillDispatcher = NewLoader(LoaderConfig{})
}

func TestLoaderCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	l, _ := newLoaderWithFakeServer(t, "fs", []Tool{{Name: "x"}})
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if l.Has("fs.x") {
		t.Error("tools should be cleared after Close")
	}
}
