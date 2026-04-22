// Package hooks dispatches lifecycle events to subprocess hooks
// using Claude Code's JSON schema. Events: PreToolUse, PostToolUse,
// UserPromptSubmit, SessionStart, SessionEnd, Stop, PreLLMCall,
// PostLLMCall, PreMemoryWrite, PostMemoryRecall, ScheduledTaskFire,
// CommitmentDue.
package hooks
