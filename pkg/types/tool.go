package types

import "encoding/json"

// ToolDef describes a tool available to the agent. Invocations go
// through exec.Cmd with a typed argv constructed from ArgvTemplate —
// no shell, so metacharacters in parameters are data.
//
// ArgvTemplate holds the args ONLY (not argv[0]). exec.Command
// supplies argv[0] implicitly from Path. A template of ["{msg}"]
// invokes Path with one argument. An empty template invokes Path
// with no arguments.
//
// Description + ParametersSchema are what the LLM sees in its
// function-calling list — without them the model can't decide
// when or how to call the tool, so these are required in practice
// even though they're optional on the struct for backward
// compatibility.
type ToolDef struct {
	Name             string          `json:"name"`
	Path             string          `json:"path"`
	ArgvTemplate     []string        `json:"argv_template"`
	Description      string          `json:"description,omitempty"`
	ParametersSchema json.RawMessage `json:"parameters_schema,omitempty"`
	Capabilities     []string        `json:"capabilities,omitempty"`

	// RecommendTools names tools to reach for INSTEAD of this one
	// where they fit, and AvoidTools names tools that should not be
	// used in this one's place.
	//
	// Both are rendered into the description from what is ACTUALLY
	// REGISTERED on the node, so a cross-reference is never made to a
	// tool the model cannot call. shell_command's description used to
	// hardcode "prefer read_file, list_files, glob, grep" — and once
	// compute.disabled_tools could switch any of those off, that
	// sentence became a standing lie the model kept acting on.
	//
	// Names, not descriptions: the point is that the LIST is derived
	// and the prose around it is not.
	RecommendTools []string `json:"recommend_tools,omitempty"`
	AvoidTools     []string `json:"avoid_tools,omitempty"`
	SidecarOnly    bool     `json:"sidecar_only,omitempty"`
	RiskTier       RiskTier `json:"risk_tier"`
}

// ToolPermission is a per-tool grant attached to a role or session.
// AllowedPaths further narrows a filesystem-touching tool to a
// subtree; empty means the tool's own default applies.
type ToolPermission struct {
	Tool         string   `json:"tool"`
	Effect       Effect   `json:"effect"`
	AllowedPaths []string `json:"allowed_paths,omitempty"`
}
