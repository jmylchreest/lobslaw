package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/tools"

	"github.com/jmylchreest/lobslaw/pkg/types"
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
	proxyURL       func(role string) string
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

	// ProxyURL returns the HTTPS_PROXY URL for a given role
	// ("mcp/<server-name>"). Wired to the egress provider's
	// SubprocessProxyURL in production. Nil → no proxy injected
	// (subprocess egresses directly; only safe when smokescreen
	// isn't running, e.g. tests).
	ProxyURL func(role string) string
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
		proxyURL:       cfg.ProxyURL,
		log:            logger,
		servers:        make(map[string]*managedServer),
		tools:          make(map[string]*managedTool),
	}
}

// proxyEnv returns HTTPS_PROXY/HTTP_PROXY/NO_PROXY env entries for
// the given server-role. Returns nil when no proxyURL function is
// wired. Both upper- and lower-case forms emitted because different
// HTTP libraries honour different casings.
func (l *Loader) proxyEnv(role string) []string {
	if l.proxyURL == nil {
		return nil
	}
	url := l.proxyURL(role)
	if url == "" {
		return nil
	}
	return []string{
		"HTTPS_PROXY=" + url,
		"https_proxy=" + url,
		"HTTP_PROXY=" + url,
		"http_proxy=" + url,
		"NO_PROXY=",
		"no_proxy=",
	}
}

// managedServer is one live MCP server the loader is responsible
// for. Keeps the client handle + resolved config so Close can
// Wait() the subprocess cleanly.
type managedServer struct {
	name    string
	command string
	args    []string
	client  *Client
	// cfg is what the server was started from, kept so a lost
	// session can be rebuilt without the operator's config being
	// re-read — and without the credential being anywhere new.
	cfg ServerConfig

	// lastErr and lastCallAt are what health is derived from. Set on
	// every invoke, so "healthy" means the last call worked rather
	// than the constant true it used to be.
	lastErr    string
	lastCallAt time.Time
}

// managedTool is one tool sourced from an MCP server. We keep both
// the server pointer (for CallTool dispatch) and the advertised
// schema (for future ToolDef generation when we wire the compute
// Registry-side integration). rawName is the tool's name as the
// upstream MCP server knows it; the Loader's tools map is keyed
// (and the Tool.Name field exposed to lobslaw) by the namespaced
// form "<server>.<rawName>" to prevent collisions across servers
// that happen to define identically-named tools.
type managedTool struct {
	tool    Tool
	rawName string
	server  *managedServer
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

// StartDirect launches MCP servers supplied directly from the
// top-level [[mcp.servers]] TOML block (no plugin manifest needed).
// Operators configure external integrations (Gmail, Slack, GitHub)
// without wrapping them in a plugin. Failures per server are
// isolated and logged.
func (l *Loader) StartDirect(ctx context.Context, servers map[string]ServerConfig) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for name, cfg := range servers {
		if cfg.Disabled {
			l.log.Info("mcp: skipping disabled server", "name", name, "source", "config")
			continue
		}
		if _, exists := l.servers[name]; exists {
			l.log.Warn("mcp: duplicate server name — keeping first", "name", name, "source", "config")
			continue
		}
		if err := l.startServerLocked(ctx, name, cfg); err != nil {
			l.log.Warn("mcp: server failed to start", "name", name, "source", "config", "err", err)
		}
	}
	return nil
}

// transportFor opens the wire to one server.
//
// Two shapes, one difference that matters: a subprocess is handed
// HTTPS_PROXY and may ignore it, while a remote server is reached
// through the egress client itself, so the role's ACL is enforced
// rather than suggested. Both carry role "mcp/<name>" either way, so
// the ACL is written once and reads the same for both.
func (l *Loader) transportFor(ctx context.Context, name string, cfg ServerConfig, env []string) (Transport, error) {
	if cfg.Remote() {
		header, err := cfg.ResolvedHeaders(l.secretResolver)
		if err != nil {
			return nil, err
		}
		return NewSSETransport(ctx, SSEConfig{
			Client: egress.ForMCP(name).HTTPClient(),
			URL:    cfg.URL,
			Header: header,
		})
	}

	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("needs either a command (stdio) or a url (remote)")
	}
	env = append(env, l.proxyEnv("mcp/"+name)...)
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	if len(env) > 0 {
		cmd.Env = env
	}
	return NewStdioTransport(cmd)
}

// startServerLocked spawns one server. Separated so tests can
// substitute a fake transport by calling the inner method with a
// pre-wired client. Caller must hold l.mu.
func (l *Loader) startServerLocked(ctx context.Context, name string, cfg ServerConfig) error {
	// Before the secret resolver and before install: a malformed
	// server should be reported as malformed, not as whichever
	// side-effect it reached first.
	if err := cfg.Validate(); err != nil {
		return err
	}

	env, err := cfg.ResolvedEnv(l.secretResolver)
	if err != nil {
		return err
	}

	if len(cfg.Install) > 0 {
		if err := l.runInstall(ctx, name, cfg.Install, env); err != nil {
			return err
		}
	}

	transport, err := l.transportFor(ctx, name, cfg, env)
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

	srv := &managedServer{
		name:    name,
		command: cfg.Command,
		args:    cfg.Args,
		client:  client,
		cfg:     cfg,
	}
	l.servers[name] = srv
	for _, t := range tools {
		raw := t.Name
		qualified := name + "." + raw
		if _, collision := l.tools[qualified]; collision {
			l.log.Warn("mcp: qualified-name collision — keeping first",
				"tool", qualified, "server", name)
			continue
		}
		// Tool exposed to the compute layer carries the namespaced
		// name so the LLM sees e.g. "gmail.search" instead of just
		// "search" (which would collide across servers).
		t.Name = qualified
		l.tools[qualified] = &managedTool{tool: t, rawName: raw, server: srv}
	}
	l.log.Info("mcp: server ready",
		"name", name, "server_name", client.ServerInfo().Name,
		"tools", len(tools))
	return nil
}

// reconnect rebuilds one server's client from the config it was
// started with.
//
// Same path as startup, deliberately: a rebuilt session has to be
// initialised and its tools re-listed, and a second construction path
// would be the one that drifts. The old client is closed first so a
// half-open stream does not outlive the entry that owned it.
func (l *Loader) reconnect(ctx context.Context, name string) error {
	l.mu.Lock()
	srv, ok := l.servers[name]
	if !ok {
		l.mu.Unlock()
		return fmt.Errorf("mcp: server %q is not running", name)
	}
	cfg := srv.cfg
	old := srv.client
	// Tools are re-registered by startServerLocked; dropping them
	// first keeps the collision check from rejecting its own entries.
	for qualified, mt := range l.tools {
		if mt.server == srv {
			delete(l.tools, qualified)
		}
	}
	delete(l.servers, name)
	err := l.startServerLocked(ctx, name, cfg)
	l.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	return err
}

// recordCallOutcome is what health is derived from.
//
// Written after any rebuild and retry, so a session that expired and
// was rebuilt reports the outcome the CALLER saw rather than the
// transient 404 in the middle — which is the honest answer and also
// the one that stops a self-healing server looking broken.
func (l *Loader) recordCallOutcome(server string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	srv, ok := l.servers[server]
	if !ok {
		return
	}
	srv.lastCallAt = time.Now()
	if err != nil {
		srv.lastErr = err.Error()
		return
	}
	srv.lastErr = ""
}

// toolByName reads one tool under the lock.
func (l *Loader) toolByName(name string) (*managedTool, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tools[name]
	return t, ok
}

// runInstall executes the configured install command before the
// server is spawned. Output is captured + logged; install timeout
// is generous (5 min) since first-run package fetches can be slow
// over a cold pip/npm cache. Subsequent boots are fast because
// uv/bun cache hits are no-ops.
func (l *Loader) runInstall(ctx context.Context, name string, install []string, env []string) error {
	if len(install) == 0 {
		return nil
	}
	installCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	env = append(env, l.proxyEnv("mcp/"+name)...)
	cmd := exec.CommandContext(installCtx, install[0], install[1:]...)
	if len(env) > 0 {
		cmd.Env = env
	}
	l.log.Info("mcp: install starting", "server", name, "command", install)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mcp: server %q install %v failed: %w (output: %s)", name, install, err, strings.TrimSpace(string(out)))
	}
	l.log.Info("mcp: install complete", "server", name)
	return nil
}

// registerServer is the test-facing hook that lets unit tests
// attach a fake transport-backed client instead of spawning a
// subprocess. Not intended for production callers.
func (l *Loader) registerServer(name string, client *Client, tools []Tool, cfg ServerConfig) {
	l.mu.Lock()
	defer l.mu.Unlock()
	srv := &managedServer{name: name, client: client, cfg: cfg}
	l.servers[name] = srv
	for _, t := range tools {
		raw := t.Name
		qualified := name + "." + raw
		t.Name = qualified
		l.tools[qualified] = &managedTool{tool: t, rawName: raw, server: srv}
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

	// Diagnostic telemetry. Arg keys only (no values) at INFO so
	// secrets/PII don't leak into logs; full arg payload + duration
	// + result size at DEBUG for deep debugging. Error path always
	// at WARN with the upstream message so operators can tell why a
	// turn went sideways without enabling debug.
	argKeys := make([]string, 0, len(req.Params))
	for k := range req.Params {
		argKeys = append(argKeys, k)
	}
	l.log.Info("mcp: tool call",
		"server", t.server.name,
		"tool", t.rawName,
		"qualified", req.Name,
		"arg_keys", argKeys)
	start := time.Now()

	res, err := t.server.client.CallTool(ctx, t.rawName, req.Params)
	if err != nil && errors.Is(err, ErrSessionGone) {
		// A remote session is not forever: the server restarts, an
		// idle stream is reaped, a proxy times the connection out.
		// The endpoint and the credential are still good, so the
		// recovery is to build a new session — not to report the
		// integration broken and wait for someone to restart the
		// node, which is what happened before this existed.
		//
		// Once. A server refusing the freshly-built session too is
		// saying something other than "that id is stale", and
		// retrying a second time would turn a bad config into a
		// loop.
		l.log.Info("mcp: session lost; rebuilding",
			"server", t.server.name, "tool", t.rawName)
		if rebuildErr := l.reconnect(ctx, t.server.name); rebuildErr != nil {
			l.log.Warn("mcp: could not rebuild the session",
				"server", t.server.name, "err", rebuildErr)
		} else if fresh, ok := l.toolByName(req.Name); ok {
			res, err = fresh.server.client.CallTool(ctx, fresh.rawName, req.Params)
		}
	}
	dur := time.Since(start)
	l.recordCallOutcome(t.server.name, err)
	if err != nil {
		l.log.Warn("mcp: tool call failed",
			"server", t.server.name,
			"tool", t.rawName,
			"duration_ms", dur.Milliseconds(),
			"err", err)
		return nil, err
	}

	resultBytes := 0
	for _, c := range res.Content {
		resultBytes += len(c.Text)
	}
	if res.IsError {
		l.log.Warn("mcp: tool returned error",
			"server", t.server.name,
			"tool", t.rawName,
			"duration_ms", dur.Milliseconds(),
			"result_bytes", resultBytes)
	} else {
		l.log.Debug("mcp: tool call ok",
			"server", t.server.name,
			"tool", t.rawName,
			"duration_ms", dur.Milliseconds(),
			"result_bytes", resultBytes)
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

// ListServers returns a snapshot view of every registered server
// for the mcp_list builtin.
//
// Health is the outcome of the last call, not a probe. MCP has no
// cheap liveness check, which is why this used to report the constant
// true — accurate about the protocol, and useless as an answer: a
// server whose session had expired listed as healthy while every call
// returned 404, and the operator reading it went looking elsewhere.
// Reporting the last outcome costs nothing and cannot mislead in that
// direction.
func (l *Loader) ListServers() []tools.MCPServerView {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]tools.MCPServerView, 0, len(l.servers))
	for name, srv := range l.servers {
		count := 0
		for _, t := range l.tools {
			if t.server == srv {
				count++
			}
		}
		view := tools.MCPServerView{
			Name:      name,
			Command:   srv.command,
			URL:       srv.cfg.URL,
			Args:      srv.args,
			ToolCount: count,
			Healthy:   srv.lastErr == "",
			LastError: srv.lastErr,
		}
		if !srv.lastCallAt.IsZero() {
			view.LastCallAt = srv.lastCallAt.UTC().Format(time.RFC3339)
		}
		out = append(out, view)
	}
	return out
}

// AddServer spawns a new MCP server at runtime. Expected shape:
// fullCmd[0] is the executable, fullCmd[1:] are args. No-op on
// duplicate name — caller surfaces that as a client-side "already
// registered" hint via the returned error.
func (l *Loader) AddServer(ctx context.Context, name string, fullCmd []string, env map[string]string) error {
	if len(fullCmd) == 0 {
		return fmt.Errorf("mcp: AddServer %q: command required", name)
	}
	l.mu.Lock()
	if _, exists := l.servers[name]; exists {
		l.mu.Unlock()
		return fmt.Errorf("mcp: server %q already registered", name)
	}
	cfg := ServerConfig{
		Command: fullCmd[0],
		Args:    fullCmd[1:],
		Env:     env,
	}
	err := l.startServerLocked(ctx, name, cfg)
	l.mu.Unlock()
	return err
}

// RemoveServer stops and deregisters a server. Returns an error
// when the name is unknown so the caller doesn't silently think
// something happened.
func (l *Loader) RemoveServer(_ context.Context, name string) error {
	l.mu.Lock()
	srv, ok := l.servers[name]
	if !ok {
		l.mu.Unlock()
		return fmt.Errorf("mcp: server %q not found", name)
	}
	delete(l.servers, name)
	for key, t := range l.tools {
		if t.server == srv {
			delete(l.tools, key)
		}
	}
	l.mu.Unlock()
	_ = srv.client.Close()
	return nil
}

// ToolDefs returns a compute-layer ToolDef per MCP-registered tool.
// Path uses the "mcp:" scheme so the compute.Executor knows to
// route through the SkillDispatcher (where the Loader lives) rather
// than exec'ing a binary. Node.New calls this after StartDirect +
// Start to populate the compute.Registry — that's what makes MCP
// tools visible to the LLM's function-calling list.
func (l *Loader) ToolDefs() []*types.ToolDef {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*types.ToolDef, 0, len(l.tools))
	for _, t := range l.tools {
		var schema []byte
		if len(t.tool.InputSchema) > 0 {
			schema = []byte(t.tool.InputSchema)
		}
		out = append(out, &types.ToolDef{
			Name:             t.tool.Name,
			Path:             "mcp:" + t.tool.Name,
			Description:      t.tool.Description,
			ParametersSchema: schema,
			RiskTier:         types.RiskCommunicating,
		})
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
