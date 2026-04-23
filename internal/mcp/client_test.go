package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
)

// memTransport is an in-memory Transport for unit tests. Canned
// responses come back in FIFO order; sent frames are captured for
// assertions.
type memTransport struct {
	mu       sync.Mutex
	sent     [][]byte
	queue    [][]byte
	closed   bool
	sendErr  error
}

func (m *memTransport) Send(_ context.Context, frame []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("closed")
	}
	if m.sendErr != nil {
		return m.sendErr
	}
	m.sent = append(m.sent, append([]byte(nil), frame...))
	return nil
}

func (m *memTransport) Recv(_ context.Context) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.queue) == 0 {
		return nil, io.EOF
	}
	out := m.queue[0]
	m.queue = m.queue[1:]
	return out, nil
}

func (m *memTransport) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

// queueResponse appends a canned JSON response. Used in test
// setup to pair up expected request → response pairs.
func (m *memTransport) queueResponse(r Response) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, _ := json.Marshal(r)
	m.queue = append(m.queue, b)
}

// lastSent returns the most recently sent frame parsed as a
// Request; convenience for assertion-heavy tests.
func (m *memTransport) lastSent(t *testing.T) Request {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		t.Fatal("no frames sent")
	}
	var r Request
	if err := json.Unmarshal(m.sent[len(m.sent)-1], &r); err != nil {
		t.Fatalf("sent frame not valid JSON: %v", err)
	}
	return r
}

// --- construction --------------------------------------------------------

func TestNewClientRequiresTransport(t *testing.T) {
	t.Parallel()
	if _, err := NewClient(Config{}); err == nil {
		t.Error("nil transport should error")
	}
}

func TestNewClientDefaultsInfo(t *testing.T) {
	t.Parallel()
	c, _ := NewClient(Config{Transport: &memTransport{}})
	if c.info.Name != "lobslaw" || c.info.Version != "dev" {
		t.Errorf("default ClientInfo: %+v", c.info)
	}
}

// --- Initialize ---------------------------------------------------------

func TestInitializeHappyPath(t *testing.T) {
	t.Parallel()
	tr := &memTransport{}
	c, _ := NewClient(Config{Transport: tr})

	tr.queueResponse(Response{
		JSONRPC: JSONRPCVersion,
		ID:      float64(1),
		Result: json.RawMessage(`{
			"protocolVersion": "2024-11-05",
			"serverInfo": {"name": "mock-server", "version": "1.0.0"},
			"capabilities": {"tools": {"listChanged": false}}
		}`),
	})

	res, err := c.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.ServerInfo.Name != "mock-server" {
		t.Errorf("server name: %q", res.ServerInfo.Name)
	}

	req := tr.lastSent(t)
	if req.Method != "initialize" {
		t.Errorf("method: %q", req.Method)
	}
	var params InitializeParams
	_ = json.Unmarshal(req.Params, &params)
	if params.ClientInfo.Name != "lobslaw" {
		t.Errorf("ClientInfo.Name: %q", params.ClientInfo.Name)
	}
	if params.ProtocolVersion != ProtocolVersion {
		t.Errorf("ProtocolVersion: %q", params.ProtocolVersion)
	}
}

func TestInitializeIdempotent(t *testing.T) {
	t.Parallel()
	tr := &memTransport{}
	c, _ := NewClient(Config{Transport: tr})
	tr.queueResponse(Response{
		JSONRPC: JSONRPCVersion,
		ID:      float64(1),
		Result:  json.RawMessage(`{"serverInfo": {"name": "s"}}`),
	})
	// First call: real handshake.
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Second call: must NOT re-send. Queue empty → if it tried, we'd
	// get EOF from Recv.
	if _, err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("second Initialize should be no-op; got %v", err)
	}
}

// --- RPC error propagation ---------------------------------------------

func TestCallPropagatesRPCError(t *testing.T) {
	t.Parallel()
	tr := &memTransport{}
	c, _ := NewClient(Config{Transport: tr})

	tr.queueResponse(Response{
		JSONRPC: JSONRPCVersion,
		ID:      float64(1),
		Error:   &RPCError{Code: -32601, Message: "method not found"},
	})
	_, err := c.Initialize(context.Background())
	if err == nil {
		t.Fatal("RPC error should propagate")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError; got %T", err)
	}
	if rpcErr.Code != -32601 {
		t.Errorf("code: %d", rpcErr.Code)
	}
}

// --- ListTools + CallTool ----------------------------------------------

func TestListToolsBeforeInitializeErrors(t *testing.T) {
	t.Parallel()
	c, _ := NewClient(Config{Transport: &memTransport{}})
	_, err := c.ListTools(context.Background())
	if err == nil {
		t.Error("ListTools pre-Initialize should error")
	}
}

func TestListToolsHappyPath(t *testing.T) {
	t.Parallel()
	tr := &memTransport{}
	c, _ := NewClient(Config{Transport: tr})
	tr.queueResponse(Response{ID: float64(1), Result: json.RawMessage(`{"serverInfo":{"name":"s"}}`)})
	_, _ = c.Initialize(context.Background())

	tr.queueResponse(Response{
		ID: float64(2),
		Result: json.RawMessage(`{"tools":[
			{"name":"read_file","description":"Read a file","inputSchema":{}},
			{"name":"write_file","description":"Write a file","inputSchema":{}}
		]}`),
	})
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("len=%d", len(tools))
	}
	if tools[0].Name != "read_file" {
		t.Errorf("tool[0]: %q", tools[0].Name)
	}
}

func TestCallToolHappyPath(t *testing.T) {
	t.Parallel()
	tr := &memTransport{}
	c, _ := NewClient(Config{Transport: tr})
	tr.queueResponse(Response{ID: float64(1), Result: json.RawMessage(`{"serverInfo":{"name":"s"}}`)})
	_, _ = c.Initialize(context.Background())

	tr.queueResponse(Response{
		ID:     float64(2),
		Result: json.RawMessage(`{"content":[{"type":"text","text":"hello"}]}`),
	})
	res, err := c.CallTool(context.Background(), "greet", map[string]any{"who": "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Error("should not report IsError")
	}
	if len(res.Content) != 1 || res.Content[0].Text != "hello" {
		t.Errorf("content: %+v", res.Content)
	}

	// Assert the request shape
	var lastCall Request
	for _, b := range tr.sent {
		var r Request
		_ = json.Unmarshal(b, &r)
		if r.Method == "tools/call" {
			lastCall = r
		}
	}
	if lastCall.Method != "tools/call" {
		t.Fatal("no tools/call request captured")
	}
	var params CallToolParams
	_ = json.Unmarshal(lastCall.Params, &params)
	if params.Name != "greet" || params.Arguments["who"] != "alice" {
		t.Errorf("params: %+v", params)
	}
}

// TestIDMismatch — a server returning an ID that doesn't match
// our request is a protocol violation; surface cleanly rather
// than treating it as success.
func TestIDMismatch(t *testing.T) {
	t.Parallel()
	tr := &memTransport{}
	c, _ := NewClient(Config{Transport: tr})
	tr.queueResponse(Response{
		ID:     float64(99), // wrong
		Result: json.RawMessage(`{}`),
	})
	_, err := c.Initialize(context.Background())
	if err == nil || !containsString(err.Error(), "id mismatch") {
		t.Errorf("want id mismatch error; got %v", err)
	}
}

// --- Close --------------------------------------------------------------

func TestCloseIdempotent(t *testing.T) {
	t.Parallel()
	tr := &memTransport{}
	c, _ := NewClient(Config{Transport: tr})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Second close — no error (transport's own Close is idempotent).
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// --- helpers -----------------------------------------------------------

func containsString(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
