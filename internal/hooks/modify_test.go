package hooks

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestModifyChainPreservesRewrites(t *testing.T) {
	dir := t.TempDir()
	seen := filepath.Join(dir, "seen.json")
	first := writeHookScript(t, dir, "first.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"command":"rewritten"}}}'`)
	second := writeHookScript(t, dir, "second.sh", `cat > "`+seen+`"; echo '{"decision":"approve"}'`)
	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: first}, {Command: second}}}, nil)
	original := map[string]string{"command": "original", "keep": "yes"}
	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_input": original})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.UpdatedInput["command"] != "rewritten" || resp.UpdatedInput["keep"] != "yes" {
		t.Fatalf("lost accumulated input: %+v", resp)
	}
	raw, err := os.ReadFile(seen)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input map[string]string `json:"tool_input"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Input["command"] != "rewritten" || payload.Input["keep"] != "yes" {
		t.Fatalf("next hook saw %v", payload.Input)
	}
	if original["command"] != "original" {
		t.Fatal("mutated caller input")
	}
}

func TestModifyRejectsInvalidResponses(t *testing.T) {
	cases := []string{
		`{"decision":"modify"}`,
		`{"decision":"modify","hookSpecificOutput":{"updatedInput":null}}`,
		`{"decision":"modify","hookSpecificOutput":{"updatedInput":[]}}`,
		`{"decision":"modify","hookSpecificOutput":{"updatedInput":{"value":1}}}`,
		`{"decision":"modify","hookSpecificOutput":{"updatedInput":{"value":true}}}`,
		`{"decision":"modify","hookSpecificOutput":{"updatedInput":{"value":null}}}`,
		`{"decision":"modify","hookSpecificOutput":{"updatedInput":{"__user_id":"root"}}}`,
		`{"decision":"modify","hookSpecificOutput":{"updatedInput":{},"tool_name":"other"}}`,
		`{"decision":"allow","args_override":{"value":"ignored"}}`,
		`{"decision":"deny"}`,
	}
	for _, response := range cases {
		t.Run(response, func(t *testing.T) {
			hook := writeHookScript(t, t.TempDir(), "hook.sh", `echo '`+response+`'`)
			d := NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
			if _, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_input": map[string]string{}}); err == nil {
				t.Fatal("invalid modification accepted")
			}
		})
	}
}

func TestModifyChainLastPatchWinsAndBlockStops(t *testing.T) {
	for _, block := range []bool{false, true} {
		t.Run(map[bool]string{false: "last patch", true: "block"}[block], func(t *testing.T) {
			dir := t.TempDir()
			first := writeHookScript(t, dir, "first.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"command":"first","added":"yes"}}}'`)
			second := writeHookScript(t, dir, "second.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"command":"second"}}}'`)
			last := writeHookScript(t, dir, "last.sh", `echo '{}'`)
			if block {
				last = writeHookScript(t, dir, "last.sh", `echo '{"decision":"block"}'`)
			}
			d := NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: first}, {Command: second}, {Command: last}}}, nil)
			resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_input": map[string]string{"command": "original"}})
			if block {
				if !errors.Is(err, types.ErrHookBlocked) {
					t.Fatalf("block lost: %v", err)
				}
				return
			}
			if err != nil || resp.UpdatedInput["command"] != "second" || resp.UpdatedInput["added"] != "yes" {
				t.Fatalf("chain: %+v %v", resp, err)
			}
		})
	}
}

func TestModifyOtherEventIsRejected(t *testing.T) {
	hook := writeHookScript(t, t.TempDir(), "hook.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{}}}'`)
	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPostToolUse: {{Command: hook}}}, nil)
	if _, err := d.Dispatch(context.Background(), types.HookPostToolUse, Payload{}); err == nil {
		t.Fatal("unsupported modification acknowledged")
	}
}

func TestStandardUpdatedInputReplacesArguments(t *testing.T) {
	hook := writeHookScript(t, t.TempDir(), "hook.sh", `echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"new"}}}'`)
	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_input": map[string]string{"command": "old", "removed": "yes"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.UpdatedInput) != 1 || resp.UpdatedInput["command"] != "new" {
		t.Fatalf("replacement: %+v", resp)
	}
}
