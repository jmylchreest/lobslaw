package types

// HookEvent names a lifecycle event observed by subprocess hooks.
// The Claude Code-named subset shares the same JSON schema so
// Claude Code plugins drop in unchanged.
type HookEvent string

// The Claude Code-compatible events. Names and payload schema match
// Claude Code exactly so an existing plugin runs unmodified; do not
// rename these to fit lobslaw's internal vocabulary.
const (
	HookPreToolUse       HookEvent = "PreToolUse"
	HookPostToolUse      HookEvent = "PostToolUse"
	HookUserPromptSubmit HookEvent = "UserPromptSubmit"
	HookSessionStart     HookEvent = "SessionStart"
	HookSessionEnd       HookEvent = "SessionEnd"
	HookStop             HookEvent = "Stop"
	HookNotification     HookEvent = "Notification"
	HookPreCompact       HookEvent = "PreCompact"

	// The lobslaw-specific events, covering the subsystems Claude Code
	// has no equivalent for: direct LLM calls, the memory layer, and
	// the scheduler.
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

// HookDecision is a hook's verdict on the event it observed. An
// empty decision means "no opinion" and leaves the flow untouched.
type HookDecision string

// The verdicts a hook can return.
const (
	// HookApprove lets the action proceed.
	HookApprove HookDecision = "approve"
	// HookBlock stops the action; Reason is surfaced to the agent.
	HookBlock HookDecision = "block"
	// HookModify lets the action proceed with the substitutions in
	// HookSpecificOutput.updatedInput applied (PreToolUse only).
	// updatedInput is a string-valued patch; omitted arguments are retained.
	HookModify HookDecision = "modify"
)

// HookResponse is the JSON a hook subprocess writes to stdout. The
// envelope uses camelCase hookSpecificOutput. PreToolUse modifications use
// updatedInput; compatibility depends on the supported decision and value types.
type HookResponse struct {
	Decision           HookDecision   `json:"decision,omitempty"`
	Reason             string         `json:"reason,omitempty"`
	HookSpecificOutput map[string]any `json:"hookSpecificOutput,omitempty"`
}
