package types

// HookEvent names a lifecycle event observed by subprocess hooks.
// The Claude Code-named subset shares the same JSON schema so
// Claude Code plugins drop in unchanged.
type HookEvent string

const (
	HookPreToolUse       HookEvent = "PreToolUse"
	HookPostToolUse      HookEvent = "PostToolUse"
	HookUserPromptSubmit HookEvent = "UserPromptSubmit"
	HookSessionStart     HookEvent = "SessionStart"
	HookSessionEnd       HookEvent = "SessionEnd"
	HookStop             HookEvent = "Stop"
	HookNotification     HookEvent = "Notification"
	HookPreCompact       HookEvent = "PreCompact"

	HookPreLLMCall        HookEvent = "PreLLMCall"
	HookPostLLMCall       HookEvent = "PostLLMCall"
	HookPreMemoryWrite    HookEvent = "PreMemoryWrite"
	HookPostMemoryRecall  HookEvent = "PostMemoryRecall"
	HookScheduledTaskFire HookEvent = "ScheduledTaskFire"
	HookCommitmentDue     HookEvent = "CommitmentDue"
)

// HookConfig registers a subprocess hook. Match is applied against
// the event payload; the most common use is matching on tool name
// for the tool-use events.
type HookConfig struct {
	Event          HookEvent         `json:"event" toml:"event"`
	Match          map[string]string `json:"match,omitempty" toml:"match,omitempty"`
	Command        string            `json:"command" toml:"command"`
	Args           []string          `json:"args,omitempty" toml:"args,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty" toml:"timeout_seconds,omitempty"`
}

type HookDecision string

const (
	HookApprove HookDecision = "approve"
	HookBlock   HookDecision = "block"
	HookModify  HookDecision = "modify"
)

type HookResponse struct {
	Decision           HookDecision   `json:"decision,omitempty"`
	Reason             string         `json:"reason,omitempty"`
	HookSpecificOutput map[string]any `json:"hookSpecificOutput,omitempty"`
}
