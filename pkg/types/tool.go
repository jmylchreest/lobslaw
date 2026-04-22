package types

// ToolDef describes a tool available to the agent. Invocations go
// through exec.Cmd with a typed argv constructed from ArgvTemplate —
// no shell, so metacharacters in parameters are data.
//
// ArgvTemplate holds the args ONLY (not argv[0]). exec.Command
// supplies argv[0] implicitly from Path. A template of ["{msg}"]
// invokes Path with one argument. An empty template invokes Path
// with no arguments.
type ToolDef struct {
	Name         string   `json:"name"`
	Path         string   `json:"path"`
	ArgvTemplate []string `json:"argv_template"`
	Capabilities []string `json:"capabilities,omitempty"`
	SidecarOnly  bool     `json:"sidecar_only,omitempty"`
	RiskTier     RiskTier `json:"risk_tier"`
}

type ToolPermission struct {
	Tool         string   `json:"tool"`
	Effect       Effect   `json:"effect"`
	AllowedPaths []string `json:"allowed_paths,omitempty"`
}
