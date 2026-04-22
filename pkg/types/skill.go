package types

import "time"

// SkillManifest is the parsed manifest.yaml for a skill. Core
// fields are OpenClaw/clawhub-compatible; Security is the
// lobslaw extension block.
type SkillManifest struct {
	ID           string         `yaml:"id" json:"id"`
	Name         string         `yaml:"name" json:"name"`
	Version      string         `yaml:"version" json:"version"`
	Description  string         `yaml:"description" json:"description"`
	Runtime      string         `yaml:"runtime" json:"runtime"`
	Schema       map[string]any `yaml:"schema" json:"schema"`
	Dependencies []string       `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
	Examples     []string       `yaml:"examples,omitempty" json:"examples,omitempty"`
	Triggers     []string       `yaml:"triggers,omitempty" json:"triggers,omitempty"`
	Security     SkillSecurity  `yaml:"security" json:"security"`
}

type SkillSecurity struct {
	Network         []string `yaml:"network,omitempty" json:"network,omitempty"`
	FS              []string `yaml:"fs,omitempty" json:"fs,omitempty"`
	Sidecar         bool     `yaml:"sidecar,omitempty" json:"sidecar,omitempty"`
	DangerousFilter []string `yaml:"dangerous_filter,omitempty" json:"dangerous_filter,omitempty"`
	AllowedEnv      []string `yaml:"allowed_env,omitempty" json:"allowed_env,omitempty"`
	MinPolicyScope  []string `yaml:"min_policy_scope,omitempty" json:"min_policy_scope,omitempty"`
	RiskTier        RiskTier `yaml:"risk_tier,omitempty" json:"risk_tier,omitempty"`
}

type SkillInvocation struct {
	SkillID string
	Params  map[string]any
	Context InvocationContext
}

type InvocationContext struct {
	Claims *Claims
	Scope  string
	Budget *TurnBudget
	Usage  *TurnUsage
	TurnID string
}

type SkillResult struct {
	Output   any
	Error    string
	Duration time.Duration
	Logs     []string
}
