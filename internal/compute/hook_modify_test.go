package compute

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/policy"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestHookModificationReachesSubprocess(t *testing.T) {
	dir := t.TempDir()
	env := newTestEnv(t, func(c *ExecutorConfig) { c.AllowedPathRoots = []string{dir} })
	tool := writeScript(t, dir, "echo.sh", `printf '%s' "$1"`)
	hook := writeScript(t, dir, "hook.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"text":"rewritten"}}}'`)
	if err := env.reg.Register(&types.ToolDef{Name: "echo", Path: tool, ArgvTemplate: []string{"{text}"}}); err != nil {
		t.Fatal(err)
	}
	env.executor.hooks = hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
	params := map[string]string{"text": "original"}
	result, err := env.executor.Invoke(context.Background(), InvokeRequest{ToolName: "echo", Params: params, Claims: &types.Claims{Scope: "owner"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Stdout), "rewritten") {
		t.Fatalf("tool saw %q", result.Stdout)
	}
	if params["text"] != "original" {
		t.Fatal("caller arguments changed")
	}
}

func TestHookRewriteCannotBypassHardline(t *testing.T) {
	env := newTestEnv(t)
	hook := writeScript(t, t.TempDir(), "hook.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"path":"/etc/shadow"}}}'`)
	env.executor.hooks = hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
	if err := env.reg.Register(&types.ToolDef{Name: "echo", Path: BuiltinScheme + "echo"}); err != nil {
		t.Fatal(err)
	}
	ran := false
	b := newTestDispatcher()
	if err := b.Register("echo", func(context.Context, map[string]string) ([]byte, int, error) { ran = true; return nil, 0, nil }); err != nil {
		t.Fatal(err)
	}
	env.executor.SetBuiltins(b)
	_, err := env.executor.Invoke(context.Background(), InvokeRequest{ToolName: "echo", Params: map[string]string{"path": "/tmp/safe"}, Claims: &types.Claims{Scope: "owner"}})
	if err == nil || ran {
		t.Fatalf("rewritten secret path executed: ran=%v err=%v", ran, err)
	}
}

func TestHookPreparedArgumentsSurviveApprovalResume(t *testing.T) {
	a, e, _ := gatedAgent(t)
	a.cfg.Provider = NewMockProvider(MockResponse{Content: "done"})
	dir := t.TempDir()
	marker := filepath.Join(dir, "count")
	hook := writeScript(t, dir, "hook.sh", `echo x >> "`+marker+`"; echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"event":"rewritten event"}}}'`)
	e.hooks = hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
	var seen string
	b := newTestDispatcher()
	if err := b.Register("memory_write", func(_ context.Context, args map[string]string) ([]byte, int, error) {
		seen = args["event"]
		return []byte("ok"), 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	e.SetBuiltins(b)
	req := confirmRequest(t)
	tc := writeCall()
	inv, pending, err := a.runToolCall(context.Background(), req, tc)
	if err != nil || pending == nil {
		t.Fatalf("first call: %v %v", pending, err)
	}
	if !strings.Contains(pending.Reason, "rewritten event") {
		t.Fatalf("prompt described original input: %s", pending.Reason)
	}
	messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{tc}}, toolResultMessage(tc, inv)}
	// A changed hook must not run when the user answers the existing prompt.
	writeScript(t, dir, "hook.sh", `echo x >> "`+marker+`"; echo '{"decision":"block","reason":"changed hook"}'`)
	_, err = a.ResumeFromConfirmation(WithTurnApproval(context.Background(), pending.Action, pending.Resource), req, messages)
	if err != nil {
		t.Fatal(err)
	}
	if seen != "rewritten event" {
		t.Fatalf("executed %q", seen)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "x\n" {
		t.Fatalf("hook reran: %q", raw)
	}
}

func TestPreparedCallCanPassTwoConfirmationGates(t *testing.T) {
	a, e, _ := gatedAgent(t, &lobslawv1.PolicyRule{Id: "confirm-tool", Subject: "*", Action: "tool:exec", Resource: "memory_write", Effect: "require_confirmation", Priority: 20})
	a.cfg.Provider = NewMockProvider(MockResponse{Content: "done"})
	ran := false
	b := newTestDispatcher()
	if err := b.Register("memory_write", func(context.Context, map[string]string) ([]byte, int, error) { ran = true; return nil, 0, nil }); err != nil {
		t.Fatal(err)
	}
	e.SetBuiltins(b)
	req, tc := confirmRequest(t), writeCall()
	inv, first, err := a.runToolCall(context.Background(), req, tc)
	if err != nil || first == nil {
		t.Fatalf("first: %v %v", first, err)
	}
	msgs := []Message{{Role: "assistant", ToolCalls: []ToolCall{tc}}, toolResultMessage(tc, inv)}
	second, err := a.ResumeFromConfirmation(WithTurnApproval(context.Background(), first.Action, first.Resource), req, msgs)
	if err != nil || second == nil || !second.NeedsConfirmation || second.ConfirmationAction != MemoryWriteAction {
		t.Fatalf("second: %+v %v", second, err)
	}
	final, err := a.ResumeFromConfirmation(WithTurnApproval(context.Background(), second.ConfirmationAction, second.ConfirmationResource), req, second.Messages)
	if err != nil || final.NeedsConfirmation || !ran {
		t.Fatalf("confirmation loop: ran=%v result=%+v err=%v", ran, final, err)
	}
	ran = false
	_, again, err := a.runToolCall(context.Background(), req, tc)
	if err != nil || again == nil || ran {
		t.Fatalf("approval escaped prepared call: ran=%v pending=%+v err=%v", ran, again, err)
	}
}

func TestPreparedCallStillChecksCurrentPolicy(t *testing.T) {
	a, e, _ := gatedAgent(t)
	req, tc := confirmRequest(t), writeCall()
	inv, pending, err := a.runToolCall(context.Background(), req, tc)
	if err != nil || pending == nil {
		t.Fatalf("prepare: %v %v", pending, err)
	}
	e.policy = nil
	e.cfg.PolicyFallback = func(context.Context, *types.Claims, string, string) (policy.Decision, error) {
		return policy.Decision{Effect: types.EffectDeny, Reason: "revoked"}, nil
	}
	next, again, err := a.runToolCallWithPrepared(WithTurnApproval(context.Background(), pending.Action, pending.Resource), req, tc, inv.prepared)
	if err != nil || again != nil || !strings.Contains(next.Error, "policy denied") {
		t.Fatalf("revocation bypassed: %+v %+v %v", next, again, err)
	}
}

func TestPreparedCallBindingAndWireIsolation(t *testing.T) {
	a, _, _ := gatedAgent(t)
	req, tc := confirmRequest(t), writeCall()
	inv, _, err := a.runToolCall(context.Background(), req, tc)
	if err != nil || inv.prepared == nil {
		t.Fatalf("prepare: %+v %v", inv, err)
	}
	for _, field := range []string{"call", "tool", "turn", "arguments"} {
		t.Run(field, func(t *testing.T) {
			p := inv.prepared.clone()
			switch field {
			case "call":
				p.CallID = "another"
			case "tool":
				p.ToolName = "other"
			case "turn":
				p.TurnID = "another"
			case "arguments":
				p.OriginalArguments = "{}"
			}
			if _, _, err := a.runToolCallWithPrepared(context.Background(), req, tc, p); err == nil {
				t.Fatal("mismatched preparation accepted")
			}
		})
	}
	raw, err := json.Marshal(toolResultMessage(tc, inv))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PreparedToolCall") {
		t.Fatal("prepared metadata leaked into generic message JSON")
	}
	var m Message
	if err := json.Unmarshal([]byte(`{"PreparedToolCall":{"ToolName":"memory_write"}}`), &m); err != nil {
		t.Fatal(err)
	}
	if m.PreparedToolCall != nil {
		t.Fatal("external JSON supplied prepared metadata")
	}
}

func TestHookRewriteRechecksSensitivePath(t *testing.T) {
	env := newTestEnv(t)
	hook := writeScript(t, t.TempDir(), "hook.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"path":"/home/u/.ssh/config"}}}'`)
	env.executor.hooks = hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
	if err := env.reg.Register(&types.ToolDef{Name: "echo", Path: BuiltinScheme + "echo"}); err != nil {
		t.Fatal(err)
	}
	_, err := env.executor.Invoke(context.Background(), InvokeRequest{ToolName: "echo", Params: map[string]string{"path": "/tmp/safe"}, Claims: &types.Claims{Scope: "owner"}})
	if !errors.Is(err, ErrRequireConfirm) || !strings.Contains(confirmationReason(err), "/home/u/.ssh/config") {
		t.Fatalf("sensitive rewrite was not confirmed: %v", err)
	}
}

func TestHookRewriteRechecksShellGate(t *testing.T) {
	e, _ := shellGatedExecutor(t, &lobslawv1.PolicyRule{Id: "allow-status", Subject: "*", Action: ShellAction, Resource: "git status", Effect: "allow", Priority: 20})
	if err := e.registry.(*testCatalogue).Register(&types.ToolDef{Name: "shell_command", Path: BuiltinScheme + "shell_command"}); err != nil {
		t.Fatal(err)
	}
	hook := writeScript(t, t.TempDir(), "hook.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"command":"git push origin main"}}}'`)
	e.hooks = hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
	_, err := e.Invoke(context.Background(), InvokeRequest{ToolName: "shell_command", Params: shellParams("git status"), Claims: &types.Claims{UserID: "alice"}})
	if !errors.Is(err, ErrRequireConfirm) || !strings.Contains(confirmationReason(err), "git push origin main") {
		t.Fatalf("rewritten command bypassed gate: %v", err)
	}
}

func TestLegacyContinuationReasksAfterHookPreparation(t *testing.T) {
	a, e, _ := gatedAgent(t)
	req, tc := confirmRequest(t), writeCall()
	hook := writeScript(t, t.TempDir(), "hook.sh", `echo '{"decision":"modify","hookSpecificOutput":{"updatedInput":{"event":"new operation"}}}'`)
	e.hooks = hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
	msgs := []Message{{Role: "assistant", ToolCalls: []ToolCall{tc}}, {Role: "tool", ToolCallID: tc.ID, Content: ErrRequireConfirm.Error()}}
	res, err := a.ResumeFromConfirmation(WithTurnApproval(context.Background(), MemoryWriteAction, "episodic"), req, msgs)
	if err != nil || !res.NeedsConfirmation || !strings.Contains(res.ConfirmationReason, "new operation") {
		t.Fatalf("legacy approval reused: %+v %v", res, err)
	}
}

func TestDeniedOriginalInputDoesNotRunHooks(t *testing.T) {
	for _, kind := range []string{"hardline", "policy"} {
		t.Run(kind, func(t *testing.T) {
			env := newTestEnv(t)
			dir := t.TempDir()
			marker := filepath.Join(dir, "hook-ran")
			hook := writeScript(t, dir, "hook.sh", `touch "`+marker+`"; echo '{}'`)
			env.executor.hooks = hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{types.HookPreToolUse: {{Command: hook}}}, nil)
			if err := env.reg.Register(&types.ToolDef{Name: "echo", Path: BuiltinScheme + "echo"}); err != nil {
				t.Fatal(err)
			}
			params := map[string]string{"path": "/etc/shadow"}
			if kind == "policy" {
				params["path"] = "/tmp/safe"
				env.executor.policy = nil
				env.executor.cfg.PolicyFallback = func(context.Context, *types.Claims, string, string) (policy.Decision, error) {
					return policy.Decision{Effect: types.EffectDeny}, nil
				}
			}
			if _, err := env.executor.Invoke(context.Background(), InvokeRequest{ToolName: "echo", Params: params, Claims: &types.Claims{Scope: "owner"}}); err == nil {
				t.Fatal("denied call accepted")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("hook ran before denial: %v", err)
			}
		})
	}
}
