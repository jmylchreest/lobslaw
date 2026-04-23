package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Transport is the wire between client and server. Production uses
// StdioTransport over an exec.Cmd; tests inject an in-memory fake
// that captures sent frames + emits canned responses.
type Transport interface {
	Send(ctx context.Context, frame []byte) error
	Recv(ctx context.Context) ([]byte, error)
	Close() error
}

// Client is a minimal JSON-RPC 2.0 client over an MCP-speaking
// transport. Single-threaded call model: Initialize → ListTools →
// CallTool (zero-or-more). Streaming / server-initiated
// notifications are deferred.
type Client struct {
	transport Transport
	info      ClientInfo
	nextID    atomic.Int64

	mu          sync.Mutex
	initialized bool
	serverInfo  ServerInfo
	serverCaps  ServerCapabilities
}

// Config bundles the client dependencies. ClientInfo defaults to
// "lobslaw" / "dev" when empty.
type Config struct {
	Transport  Transport
	ClientInfo ClientInfo
}

// NewClient constructs a client. No server traffic yet — Initialize
// is where the handshake happens.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Transport == nil {
		return nil, errors.New("mcp: Transport required")
	}
	info := cfg.ClientInfo
	if info.Name == "" {
		info.Name = "lobslaw"
	}
	if info.Version == "" {
		info.Version = "dev"
	}
	return &Client{transport: cfg.Transport, info: info}, nil
}

// Close tears down the transport. Safe to call more than once.
func (c *Client) Close() error {
	if c.transport == nil {
		return nil
	}
	return c.transport.Close()
}

// Initialize performs the MCP handshake exactly once per client;
// subsequent calls return the cached result.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	c.mu.Lock()
	if c.initialized {
		out := &InitializeResult{
			ProtocolVersion: ProtocolVersion,
			Capabilities:    c.serverCaps,
			ServerInfo:      c.serverInfo,
		}
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	params := InitializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    ClientCapabilities{},
		ClientInfo:      c.info,
	}
	raw, err := c.call(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}
	var res InitializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp: initialize response: %w", err)
	}

	c.mu.Lock()
	c.initialized = true
	c.serverCaps = res.Capabilities
	c.serverInfo = res.ServerInfo
	c.mu.Unlock()
	return &res, nil
}

// ListTools fetches the server's tool catalogue. The server must
// have advertised Tools in its capabilities during Initialize; an
// empty capability surface would usually mean the server doesn't
// expose tools at all and ListTools will return an empty list with
// no error.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if !c.isInitialized() {
		return nil, errors.New("mcp: ListTools before Initialize")
	}
	raw, err := c.call(ctx, "tools/list", struct{}{})
	if err != nil {
		return nil, err
	}
	var res ListToolsResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp: tools/list response: %w", err)
	}
	return res.Tools, nil
}

// CallTool invokes a tool by name with the given arguments. The
// result's IsError field reflects a tool-logical failure (e.g. the
// tool reports "file not found") — transport failures come back as
// a non-nil err.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (*CallToolResult, error) {
	if !c.isInitialized() {
		return nil, errors.New("mcp: CallTool before Initialize")
	}
	raw, err := c.call(ctx, "tools/call", CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return nil, err
	}
	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp: tools/call response: %w", err)
	}
	return &res, nil
}

// ServerInfo returns the cached server identity after Initialize.
// Zero value before.
func (c *Client) ServerInfo() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.serverInfo
}

func (c *Client) isInitialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized
}

// call is the shared request/response primitive used by all public
// methods. Not concurrency-safe by design — MCP-over-stdio is
// sequential per client.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)

	var paramsRaw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal params: %w", err)
		}
		paramsRaw = b
	}
	req := Request{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Method:  method,
		Params:  paramsRaw,
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: marshal request: %w", err)
	}
	if err := c.transport.Send(ctx, frame); err != nil {
		return nil, fmt.Errorf("mcp: send %s: %w", method, err)
	}

	respFrame, err := c.transport.Recv(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: recv %s: %w", method, err)
	}
	var resp Response
	if err := json.Unmarshal(respFrame, &resp); err != nil {
		return nil, fmt.Errorf("mcp: parse response: %w", err)
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	if !idMatches(resp.ID, id) {
		return nil, fmt.Errorf("mcp: response id mismatch: got %v want %d", resp.ID, id)
	}
	return resp.Result, nil
}

// idMatches compares server-echoed IDs tolerantly. JSON can lose
// precision on int64 ↔ float64 round-trips, so we accept either
// numeric form as long as the integer value matches.
func idMatches(gotID any, wantID int64) bool {
	switch v := gotID.(type) {
	case int64:
		return v == wantID
	case int:
		return int64(v) == wantID
	case float64:
		return int64(v) == wantID
	case json.Number:
		n, err := v.Int64()
		return err == nil && n == wantID
	}
	return false
}

// StdioTransport is the production transport: speaks to the MCP
// server's stdin + reads newline-delimited JSON frames from its
// stdout. Stderr is optionally wired to a log sink so operator
// diagnostics aren't silently dropped.
type StdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	closed atomic.Bool
}

// NewStdioTransport spawns cmd and wires its stdio pipes. The cmd
// must not have been Started yet — we call Start here.
func NewStdioTransport(cmd *exec.Cmd) (*StdioTransport, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: stdin pipe: %w", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("mcp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = in.Close()
		return nil, fmt.Errorf("mcp: start: %w", err)
	}
	return &StdioTransport{
		cmd:    cmd,
		stdin:  in,
		stdout: bufio.NewReader(out),
	}, nil
}

// Send writes frame + newline to stdin. The ctx deadline propagates
// via exec.CommandContext cancellation the caller set up; per-Send
// deadlines aren't enforced at this layer.
func (t *StdioTransport) Send(_ context.Context, frame []byte) error {
	if t.closed.Load() {
		return errors.New("mcp: transport closed")
	}
	if _, err := t.stdin.Write(append(frame, '\n')); err != nil {
		return err
	}
	return nil
}

// Recv reads one newline-delimited frame from stdout.
func (t *StdioTransport) Recv(_ context.Context) ([]byte, error) {
	if t.closed.Load() {
		return nil, errors.New("mcp: transport closed")
	}
	line, err := t.stdout.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	return line, nil
}

// Close shuts down the subprocess. Stdin is closed first (which
// many MCP servers interpret as "exit cleanly"); Wait collects the
// exit status. Idempotent.
func (t *StdioTransport) Close() error {
	if !t.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = t.stdin.Close()
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Wait()
	}
	return nil
}
