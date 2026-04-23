package compute

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestBuiltinsRegisterRejectsEmptyName(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := b.Register("", nil); err == nil {
		t.Error("empty name should fail")
	}
}

func TestBuiltinsRegisterRejectsNilHandler(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := b.Register("x", nil); err == nil {
		t.Error("nil handler should fail")
	}
}

func TestBuiltinsRegisterRejectsDuplicate(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	noop := func(context.Context, map[string]string) ([]byte, int, error) { return nil, 0, nil }
	if err := b.Register("x", noop); err != nil {
		t.Fatal(err)
	}
	if err := b.Register("x", noop); err == nil {
		t.Error("duplicate should fail")
	}
}

func TestIsBuiltinPath(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		name    string
		builtin bool
	}{
		"builtin:current_time": {"current_time", true},
		"builtin:":             {"", true},
		"/bin/ls":              {"", false},
		"/usr/local/bin/rtk":   {"", false},
		"":                     {"", false},
	}
	for path, want := range cases {
		name, got := isBuiltinPath(path)
		if got != want.builtin {
			t.Errorf("isBuiltinPath(%q).builtin = %v; want %v", path, got, want.builtin)
		}
		if name != want.name {
			t.Errorf("isBuiltinPath(%q).name = %q; want %q", path, name, want.name)
		}
	}
}

// TestExecutorDispatchesBuiltin — register a builtin, register a
// ToolDef pointing at "builtin:echo", invoke via the Executor's
// full path (policy + hook sites fire, but the dispatch resolves
// to the in-process handler rather than exec).
func TestExecutorDispatchesBuiltin(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	b := NewBuiltins()
	_ = b.Register("echo", func(_ context.Context, args map[string]string) ([]byte, int, error) {
		return []byte("hello " + args["name"]), 0, nil
	})
	env.executor.SetBuiltins(b)

	_ = env.reg.Register(&types.ToolDef{
		Name:     "echo",
		Path:     BuiltinScheme + "echo",
		RiskTier: types.RiskReversible,
	})

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "echo",
		Params:   map[string]string{"name": "alice"},
		Claims:   &types.Claims{UserID: "u", Scope: "test"},
		TurnID:   "t",
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d; want 0", res.ExitCode)
	}
	if string(res.Stdout) != "hello alice" {
		t.Errorf("Stdout = %q; want hello alice", res.Stdout)
	}
}

// TestExecutorBuiltinErrorGoesToStderr — a builtin that returns an
// error surfaces the message as stderr + non-zero exit, not as a
// top-level Invoke error (so the agent's loop can feed it back to
// the LLM the same way a failed subprocess would).
func TestExecutorBuiltinErrorGoesToStderr(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	b := NewBuiltins()
	_ = b.Register("boom", func(context.Context, map[string]string) ([]byte, int, error) {
		return nil, 2, errors.New("bang")
	})
	env.executor.SetBuiltins(b)
	_ = env.reg.Register(&types.ToolDef{Name: "boom", Path: BuiltinScheme + "boom", RiskTier: types.RiskReversible})

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "boom",
		Claims:   &types.Claims{Scope: "test"},
		TurnID:   "t",
	})
	if err != nil {
		t.Fatalf("Invoke should not return top-level error: %v", err)
	}
	if res.ExitCode != 2 {
		t.Errorf("ExitCode = %d; want 2", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "bang") {
		t.Errorf("Stderr should contain the error message; got %q", res.Stderr)
	}
}

func TestExecutorBuiltinRejectedWhenRegistryMissing(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	_ = env.reg.Register(&types.ToolDef{Name: "x", Path: BuiltinScheme + "x", RiskTier: types.RiskReversible})

	// No SetBuiltins → Invoke should fail with ErrToolPathInvalid.
	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "x",
		Claims:   &types.Claims{Scope: "test"},
		TurnID:   "t",
	})
	if !errors.Is(err, ErrToolPathInvalid) {
		t.Errorf("want ErrToolPathInvalid; got %v", err)
	}
}

func TestStdlibCurrentTimeBuiltin(t *testing.T) {
	t.Parallel()
	stdout, exit, err := currentTimeBuiltin(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Errorf("exit = %d; want 0", exit)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout, &payload); err != nil {
		t.Fatalf("output not JSON: %v (body=%s)", err, stdout)
	}
	for _, key := range []string{"utc", "local", "zone", "offset_secs", "unix"} {
		if _, ok := payload[key]; !ok {
			t.Errorf("missing key %q in output", key)
		}
	}
}
