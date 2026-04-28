package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// MCPRegistry abstracts the subset of mcp.Loader the management
// builtins need. The real implementation lives in internal/mcp and
// satisfies this via method names; tests inject a fake. Keeping it
// as an interface in the compute package prevents a cyclic
// dependency (compute can't import mcp because mcp already imports
// compute for SkillDispatcher).
type MCPRegistry interface {
	ListServers() []MCPServerView
	AddServer(ctx context.Context, name string, cmd []string, env map[string]string) error
	RemoveServer(ctx context.Context, name string) error
}

// MCPServerView is the shape list_mcp_servers returns per server.
// Deliberately omits Env so stored secrets never reach the LLM.
type MCPServerView struct {
	Name      string   `json:"name"`
	Command   string   `json:"command"`
	Args      []string `json:"args,omitempty"`
	ToolCount int      `json:"tool_count"`
	Healthy   bool     `json:"healthy"`
}

// MCPManagementConfig wires the builtins. Nil registry skips
// registration — single-node deployments without MCP enabled won't
// advertise these tools.
type MCPManagementConfig struct {
	Registry MCPRegistry
}

// RegisterMCPManagementBuiltins installs mcp_list / mcp_add /
// mcp_remove. Intended for operators who'd rather say "add a Gmail
// MCP server" than edit config.toml directly. Changes made through
// these builtins are node-local today; Raft replication is a
// planned follow-up (see lobslaw-mcp-integration decision).
func RegisterMCPManagementBuiltins(b *Builtins, cfg MCPManagementConfig) error {
	if cfg.Registry == nil {
		return errors.New("mcp management: Registry required")
	}
	if err := b.Register("mcp_list", newMCPListHandler(cfg.Registry)); err != nil {
		return err
	}
	if err := b.Register("mcp_add", newMCPAddHandler(cfg.Registry)); err != nil {
		return err
	}
	return b.Register("mcp_remove", newMCPRemoveHandler(cfg.Registry))
}

func MCPManagementToolDefs() []*types.ToolDef {
	return []*types.ToolDef{
		{
			Name:        "mcp_list",
			Path:        BuiltinScheme + "mcp_list",
			Description: "List MCP (Model Context Protocol) servers running on this node. Returns {name, command, args, tool_count, healthy} per server. Use when the user asks what integrations are available; always call this before mcp_add so you can pick a unique name. Present as a markdown table.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {},
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
		{
			Name:        "mcp_add",
			Path:        BuiltinScheme + "mcp_add",
			Description: "Register and start an MCP server on this node. name becomes the tool namespace (name='gmail' → tools appear as 'gmail.search' etc). command is the executable; args is its argv. env is a map of plaintext env var name → value. Secrets should be passed via secret_env (map of env var name → 'env:ENV_NAME' or similar ref) so plaintext doesn't flow through the LLM. Node-local today — add to config.toml [mcp.servers] for persistence.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Logical name; becomes the tool namespace prefix."},
					"command": {"type": "string", "description": "Executable path or name on PATH."},
					"args": {"type": "array", "items": {"type": "string"}, "description": "argv for the command."},
					"env": {"type": "object", "additionalProperties": {"type": "string"}, "description": "Plaintext env vars (no secrets)."}
				},
				"required": ["name", "command"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskCommunicating,
		},
		{
			Name:        "mcp_remove",
			Path:        BuiltinScheme + "mcp_remove",
			Description: "Stop and deregister an MCP server. Use when an integration is no longer wanted or is misbehaving. Tools it provided disappear from the LLM's function list.",
			ParametersSchema: []byte(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Server name passed to mcp_add."}
				},
				"required": ["name"],
				"additionalProperties": false
			}`),
			RiskTier: types.RiskReversible,
		},
	}
}

func newMCPListHandler(reg MCPRegistry) BuiltinFunc {
	return func(_ context.Context, _ map[string]string) ([]byte, int, error) {
		servers := reg.ListServers()
		out, err := json.Marshal(map[string]any{
			"count":   len(servers),
			"servers": servers,
		})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}

func newMCPAddHandler(reg MCPRegistry) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["name"])
		if name == "" {
			return nil, 2, errors.New("mcp_add: name is required")
		}
		if strings.ContainsAny(name, "./\\ ") {
			return nil, 2, fmt.Errorf("mcp_add: name %q must not contain '.', '/', '\\' or spaces (it's used as the tool-name prefix)", name)
		}
		command := strings.TrimSpace(args["command"])
		if command == "" {
			return nil, 2, errors.New("mcp_add: command is required")
		}
		var argv []string
		if raw, ok := args["args"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &argv); err != nil {
				return nil, 2, fmt.Errorf("mcp_add: args must be a JSON array of strings: %w", err)
			}
		}
		var env map[string]string
		if raw, ok := args["env"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &env); err != nil {
				return nil, 2, fmt.Errorf("mcp_add: env must be a JSON object of strings: %w", err)
			}
		}
		fullCmd := append([]string{command}, argv...)
		if err := reg.AddServer(ctx, name, fullCmd, env); err != nil {
			return nil, 1, fmt.Errorf("mcp_add: %w", err)
		}
		out, _ := json.Marshal(map[string]any{
			"name":    name,
			"command": command,
			"args":    argv,
		})
		return out, 0, nil
	}
}

func newMCPRemoveHandler(reg MCPRegistry) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		name := strings.TrimSpace(args["name"])
		if name == "" {
			return nil, 2, errors.New("mcp_remove: name is required")
		}
		if err := reg.RemoveServer(ctx, name); err != nil {
			return nil, 1, fmt.Errorf("mcp_remove: %w", err)
		}
		out, _ := json.Marshal(map[string]any{"name": name, "removed": true})
		return out, 0, nil
	}
}
