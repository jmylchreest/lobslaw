package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// Loader discovers .mcp.json manifests, spawns each declared
// server, and presents their tools to the agent via a
// compute.SkillDispatcher — MCP tools are dispatched through the
// same path skills are (name-based, JSON params, stdout/stderr
// capture), which keeps the agent's mental model uniform and reuses
// the per-turn budget accounting.
type Loader struct {
	secretResolver SecretResolver
	clientInfo     ClientInfo
	log            *slog.Logger

	mu      sync.Mutex
	servers map[string]*managedServer // serverName → running client
	tools   map[string]*managedTool   // toolName → owning server + metadata
}

// LoaderConfig wires the loader's dependencies. ClientInfo defaults
// to "lobslaw"/"dev" when empty; SecretResolver is required when
// any discovered manifest uses secret_env.
type LoaderConfig struct {
	SecretResolver SecretResolver
	ClientInfo     ClientInfo
	Logger         *slog.Logger
}

// NewLoader constructs an empty loader. No servers are spawned
// until Start is called.
func NewLoader(cfg LoaderConfig) *Loader {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Loader{
		secretResolver: cfg.SecretResolver,
		clientInfo:     cfg.ClientInfo,
		log:            logger,
		servers:        make(map[string]*managedServer),
		tools:          make(map[string]*managedTool),
	}
}

// managedServer is one live MCP server the loader is responsible
// for. Keeps the client handle + resolved config so Close can
// Wait() the subprocess cleanly.
type managedServer struct {
	name   string
	client *Client
}

// managedTool is one tool sourced from an MCP server. We keep both
// the server pointer (for CallTool dispatch) and the advertised
// schema (for future ToolDef generation when we wire the compute
// Registry-side integration).
type managedTool struct {
	tool   Tool
	server *managedServer
}

// Start launches every discovered-and-enabled server, initializes
// each, and catalogues its tools. Errors from one server are
// logged + isolate that server — other servers still come up so
// a misconfigured plugin doesn't take down the whole MCP layer.
func (l *Loader) Start(ctx context.Context, discovered []DiscoveredManifest) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, d := range discovered {
		for name, cfg := range d.Manifest.MCPServers {
			if cfg.Disabled {
				l.log.Info("mcp: skipping disabled server", "name", name, "plugin", d.PluginDir)
				continue
			}
			if _, exists := l.servers[name]; exists {
				l.log.Warn("mcp: duplicate server name — keeping first",
					"name", name, "plugin", d.PluginDir)
				continue
			}
			if err := l.startServerLocked(ctx, name, cfg); err != nil {
				l.log.Warn("mcp: server failed to start",
					"name", name, "plugin", d.PluginDir, "err", err)
			}
		}
	}
	return nil
}

// startServerLocked spawns one server. Separated so tests can
// substitute a fake transport by calling the inner method with a
// pre-wired client. Caller must hold l.mu.
func (l *Loader) startServerLocked(ctx context.Context, name string, cfg ServerConfig) error {
	env, err := cfg.ResolvedEnv(l.secretResolver)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	transport, err := NewStdioTransport(cmd)
	if err != nil {
		return fmt.Errorf("mcp: server %q: %w", name, err)
	}
	client, err := NewClient(Config{Transport: transport, ClientInfo: l.clientInfo})
	if err != nil {
		_ = transport.Close()
		return fmt.Errorf("mcp: server %q: %w", name, err)
	}
	if _, err := client.Initialize(ctx); err != nil {
		_ = client.Close()
		return fmt.Errorf("mcp: server %q initialize: %w", name, err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("mcp: server %q tools/list: %w", name, err)
	}

	srv := &managedServer{name: name, client: client}
	l.servers[name] = srv
	for _, t := range tools {
		if _, collision := l.tools[t.Name]; collision {
			l.log.Warn("mcp: tool name collision — keeping first",
				"tool", t.Name, "server", name)
			continue
		}
		l.tools[t.Name] = &managedTool{tool: t, server: srv}
	}
	l.log.Info("mcp: server ready",
		"name", name, "server_name", client.ServerInfo().Name,
		"tools", len(tools))
	return nil
}

// registerServer is the test-facing hook that lets unit tests
// attach a fake transport-backed client instead of spawning a
// subprocess. Not intended for production callers.
func (l *Loader) registerServer(name string, client *Client, tools []Tool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	srv := &managedServer{name: name, client: client}
	l.servers[name] = srv
	for _, t := range tools {
		l.tools[t.Name] = &managedTool{tool: t, server: srv}
	}
}

// Close tears down every server. Best-effort — individual Close
// errors are logged but don't short-circuit the sweep. Idempotent.
func (l *Loader) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, srv := range l.servers {
		if err := srv.client.Close(); err != nil {
			l.log.Warn("mcp: server close failed", "name", name, "err", err)
		}
	}
	l.servers = make(map[string]*managedServer)
	l.tools = make(map[string]*managedTool)
	return nil
}

// Has satisfies compute.SkillDispatcher. Reports whether name is
// known to any running MCP server's tool catalogue.
func (l *Loader) Has(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.tools[name]
	return ok
}

// Invoke dispatches a tool call to the owning MCP server. Errors
// from the wire (transport, protocol, malformed response) surface
// as Go errors so the agent records them as tool-call errors; a
// tool that logically failed (IsError=true in the MCP response)
// comes back with ExitCode=1 and its content joined into Stderr so
// the LLM sees the failure message.
func (l *Loader) Invoke(ctx context.Context, req compute.SkillInvokeRequest) (*compute.SkillInvokeResult, error) {
	l.mu.Lock()
	t, ok := l.tools[req.Name]
	l.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("mcp: tool %q not found", req.Name)
	}
	res, err := t.server.client.CallTool(ctx, req.Name, req.Params)
	if err != nil {
		return nil, err
	}
	var stdout, stderr []byte
	for _, chunk := range res.Content {
		if chunk.Type == "text" {
			if res.IsError {
				stderr = append(stderr, chunk.Text...)
			} else {
				stdout = append(stdout, chunk.Text...)
			}
		}
	}
	exit := 0
	if res.IsError {
		exit = 1
	}
	return &compute.SkillInvokeResult{
		ExitCode: exit,
		Stdout:   stdout,
		Stderr:   stderr,
	}, nil
}

// ListTools returns every registered tool across every running
// server. Callers use this to enumerate what the agent should see
// in its tool catalogue.
func (l *Loader) ListTools() []Tool {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Tool, 0, len(l.tools))
	for _, t := range l.tools {
		out = append(out, t.tool)
	}
	return out
}

// Compile-time check that *Loader satisfies compute.SkillDispatcher
// — if the contract drifts, the build breaks here rather than at
// node.go's wiring site.
var _ compute.SkillDispatcher = (*Loader)(nil)

// ErrNotInitialized surfaces from operations that assume Start has
// already been called — reserved for future non-idempotent methods.
var ErrNotInitialized = errors.New("mcp: loader not initialized")
