---
sidebar_position: 2
---

# Hooks

Hooks run operator-configured subprocesses with JSON on stdin and optional JSON on stdout. `PreToolUse` hooks can block an executor call or rewrite its arguments. This applies to builtin and subprocess tools dispatched by the executor; the separate skill/MCP dispatch path does not gain hook support from this feature.

## Configuration

```toml
[[hooks.PreToolUse]]
command = "/opt/lobslaw/hooks/rewrite.sh"
args = []
timeout_seconds = 5
match = { tool_name = "shell_command" }

[[hooks.PostToolUse]]
command = "/opt/lobslaw/hooks/audit.sh"
timeout_seconds = 5
```

`match` is a map of exact string comparisons against input fields. All entries must match. Omit it to match every invocation for that event. Matching does not support glob patterns.

## Input

A `PreToolUse` subprocess receives:

```json
{
  "session_id": "turn-123",
  "hook_event_name": "PreToolUse",
  "tool_name": "shell_command",
  "tool_input": {"command": "git status"},
  "cwd": "/workspace",
  "actor_scope": "owner"
}
```

`session_id` currently carries the turn ID. `tool_input` contains the executor's string-valued arguments. Identity and authorization claims are carried separately and cannot be changed by an argument patch.

## Decisions and argument rewrites

Use `approve`, `block`, or the Lobslaw `modify` extension. Empty stdout or an empty decision means no opinion. Exit code 2 blocks with stderr as its reason; other nonzero exits and invalid responses fail the pre-hook chain.

```json
{
  "decision": "modify",
  "hookSpecificOutput": {
    "updatedInput": {"command": "rtk git status"}
  }
}
```

`updatedInput` is a patch: supplied keys replace or add values, omitted keys remain unchanged. Values must be strings; encode structured tool arguments as JSON strings if that tool expects them. Numbers, booleans, objects, arrays and null values are rejected. Empty and `__`-prefixed keys are reserved. There is no deletion operation; an empty string is a value, not deletion.

The standard Claude Code form is also accepted:

```json
{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"rtk git status"}}}
```

Without `decision: "modify"`, `updatedInput` **replaces** the complete input object: omitted keys are removed. The same string-value and reserved-key restrictions apply. `permissionDecision: "deny"` blocks; `allow` never bypasses local policy. `permissionDecisionReason` supplies the reason. Interactive `ask` is currently unsupported and fails explicitly; use Lobslaw's policy confirmation gates. A rewrite cannot change the tool name, caller claims, working directory or process environment. The formerly documented `args_override` spelling and `allow`/`deny` decisions are unsupported and now fail explicitly instead of silently doing nothing.

Hooks run in configuration order. Each hook sees the effective input produced by earlier hooks. Later patches win for overlapping keys; an approval or empty response preserves accumulated changes. A block stops the chain and the tool does not execute:

```json
{"decision": "block", "reason": "command rejected by local policy"}
```

Only `PreToolUse` supports modification. `PostToolUse` receives the executed `tool_input` and is advisory: its errors cannot undo an already executed tool, and its output is not applied to tool results.

## Safety and confirmation

The executor checks the original input against the hard safety rules and rejects tool-policy denials before running pre-hooks. It checks the effective input again afterwards, then evaluates current policy, sensitive-path confirmation and per-tool gates before execution. An `approve` hook response does not grant authorization or waive those checks.

Confirmation prompts describe the effective operation. The prepared arguments are retained in the existing durable continuation, together with answered gates scoped to that exact call. Approving a paused call does not rerun pre-hooks: it executes the prepared input after checking current policy and safety rules. A newly configured policy denial still stops it. The saved arguments are not sent to the model as preparation metadata and cannot be supplied through model argument JSON.

If another gate asks a second question, the first answer remains valid only for that prepared call. Subsequent calls need their own approvals unless an existing conversation grant or policy rule permits them.

Old continuations without prepared input remain readable. If pre-hooks are configured, they are prepared again and any required confirmation is asked again; an old answer is not applied to newly rewritten arguments. The existing tool-call arguments also carry the effective input, so older readers cannot fall back to the original operation. Upgrade all compute nodes before enabling rewriting: older binaries still lack hook preparation and approval tracking.

## Examples

### Reject outgoing text containing an AWS access key pattern

```sh
#!/bin/sh
input=$(cat)
text=$(printf '%s' "$input" | jq -r '.tool_input.text // .tool_input.body // ""')
if printf '%s' "$text" | grep -Eq 'AKIA[0-9A-Z]{16}'; then
  printf '%s\n' '{"decision":"block","reason":"AWS key pattern detected"}'
fi
```

### Audit a completed tool call

```sh
#!/bin/sh
input=$(cat)
logger -t lobslaw "$(printf '%s' "$input" | jq -c '{hook_event_name, tool_name, session_id}')"
```

## Timeouts

`timeout_seconds` defaults to 5 seconds. A pre-hook timeout fails the call; an outer context cancellation also stops the hook. Keep hooks short and avoid interactive subprocesses: use the normal confirmation gates to ask the user.

## Reference

- `internal/hooks/dispatcher.go` — matching, subprocess execution and ordered chaining
- `internal/hooks/modify.go` — argument replacement and patch validation
- `internal/compute/executor.go` — safety checks, gates and dispatch
- `pkg/types/hook.go` — event and response types
