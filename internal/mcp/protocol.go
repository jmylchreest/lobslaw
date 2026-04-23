package mcp

import "encoding/json"

// ProtocolVersion is what the client advertises during initialize.
// MCP has pinned this to "2024-11-05" as a stable release; servers
// that only support later negotiation should still accept an older
// client announcing an earlier version.
const ProtocolVersion = "2024-11-05"

// JSONRPCVersion is the fixed JSON-RPC version string MCP uses.
const JSONRPCVersion = "2.0"

// Request is a JSON-RPC 2.0 request. ID is required for calls that
// expect a response (we don't send notifications yet).
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response envelope. Exactly one of
// Result / Error is populated.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is JSON-RPC's error object. Code follows the standard
// range conventions (see https://www.jsonrpc.org/specification#error_object).
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error satisfies the error interface so callers can return RPCError
// values as regular Go errors.
func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

// InitializeParams are what the client sends in the initialize
// request. ClientInfo identifies lobslaw; Capabilities advertises
// what this client supports so the server can tailor its responses.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      ClientInfo         `json:"clientInfo"`
}

// ClientCapabilities is minimal in MVP — no sampling or roots support
// yet. Expansion surfaces as new fields here so servers can light up
// extra features without a breaking protocol change.
type ClientCapabilities struct {
	Experimental map[string]json.RawMessage `json:"experimental,omitempty"`
}

// ClientInfo identifies the client across the connection — servers
// can log or reject based on this.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeResult is what the server returns. Captures the server's
// advertised protocol version + its name for logs.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
}

// ServerCapabilities is the counterpart to ClientCapabilities. Only
// Tools is consumed today; resources / prompts / logging / sampling
// are reserved for future expansion.
type ServerCapabilities struct {
	Tools     *ToolsCapability          `json:"tools,omitempty"`
	Resources map[string]json.RawMessage `json:"resources,omitempty"`
	Prompts   map[string]json.RawMessage `json:"prompts,omitempty"`
}

// ToolsCapability is currently a marker — the server either
// supports tools/list + tools/call or it doesn't. ListChanged hints
// that tools/list results may change during the session; we don't
// subscribe to changes yet so the hint is informational.
type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// ServerInfo identifies the server for logs / diagnostics.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Tool is one entry returned by tools/list. Name is the invocable
// identifier; Description is for the LLM's tool-selection context;
// InputSchema is a JSON Schema the agent uses to constrain + shape
// the arguments it sends.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ListToolsResult is the tools/list response shape.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// CallToolParams are what the client sends to invoke a tool.
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// CallToolResult is the tools/call response. IsError=true means the
// tool logically failed (not a transport error); Content holds the
// structured output the LLM will see.
type CallToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// ToolContent is one chunk of the tool's response. Only Text is
// surfaced to the agent today — image / resource content types are
// deferred.
type ToolContent struct {
	Type string `json:"type"` // "text" | "image" | "resource"
	Text string `json:"text,omitempty"`
}
