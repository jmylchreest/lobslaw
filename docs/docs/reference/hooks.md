---
sidebar_position: 2
---

# Hooks

Hooks let operators inject code into the agent loop at specific points. Each hook is a shell command that receives JSON on stdin and is allowed to modify or block the next action.

## Events

| Event | Fires | Can block? |
|---|---|---|
| `SessionStart` | Once at node boot | yes (boot fails) |
| `PreToolUse` | Before every tool dispatch | yes (call denied) |
| `PostToolUse` | After every tool returns | no (advisory) |
| `UserPromptSubmit` | Before agent reads a user message | yes (turn rejected) |

## Configuration

```toml
[[hooks.PreToolUse]]
match   = "tool:exec:gws-workspace.gmail.send"
command = ["./hooks/confirm-send.sh"]
timeout = "10s"

[[hooks.PostToolUse]]
match   = "tool:exec:*"
command = ["./hooks/log-tool-call.sh"]
```

`match` patterns:

- Exact: `tool:exec:gws-workspace.gmail.send`
- Glob: `tool:exec:*` (every tool), `tool:exec:gws-*` (every gws skill tool)
- Empty / `*` — every event of that type

## Input + output

Stdin (JSON):

```json
{
  "event": "PreToolUse",
  "tool": "gws-workspace.gmail.send",
  "args": {"to": "...", "subject": "...", "body": "..."},
  "claims": {"scope": "owner", "user_id": "alice", "channel": "telegram"},
  "turn_id": "01HX...",
  "ts": "2026-04-28T13:45:01Z"
}
```

Stdout (optional, JSON):

```json
{
  "decision": "allow",                  // allow | deny
  "reason": "...",                       // shown to the agent
  "args_override": {"to": "...", ...}    // only PreToolUse
}
```

If stdout is empty, hook is advisory only — the action proceeds.

If `decision: "deny"`, the action is blocked. For PreToolUse, the agent gets the reason as a tool error and can retry / explain to the user.

## Common patterns

### Confirm before sending mail

```sh
#!/bin/sh
input=$(cat)
to=$(echo "$input" | jq -r '.args.to')
subj=$(echo "$input" | jq -r '.args.subject')

# ask via desktop notification + text reply
zenity --question --text="Send email to $to with subject '$subj'?"
case $? in
  0) echo '{"decision": "allow"}' ;;
  *) echo '{"decision": "deny", "reason": "user declined"}' ;;
esac
```

For most operators this is overkill — `[[policy.rules]] effect = "require_confirmation"` is the simpler equivalent. Hooks are for non-trivial logic (rate limits, content scans, integration with external approval systems).

### Audit every tool call to syslog

```sh
#!/bin/sh
input=$(cat)
logger -t lobslaw "$(echo "$input" | jq -c '{event, tool, args, claims}')"
```

PostToolUse, no decision — just logs.

### Block content with secrets

```sh
#!/bin/sh
input=$(cat)
text=$(echo "$input" | jq -r '.args.text // .args.body // ""')

if echo "$text" | grep -E 'AKIA[0-9A-Z]{16}' > /dev/null; then
  echo '{"decision": "deny", "reason": "AWS key pattern detected in outbound text"}'
fi
```

Useful as a backstop on `notify` and `gmail.send`.

## Timeouts

Hook commands have a per-event timeout (default 10s). Exceeded → action denied with a "hook timeout" error. Don't put long-running work in a hook; queue and return.

## Reference

- `internal/hooks/dispatcher.go` — event registry, command spawn, timeout enforcement
- `pkg/types/hooks.go` — event constants, input/output schema
