//go:build linux

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// The production binary dispatches this before starting the node. Give the
// test binary the same child-only entry point so Apply can reexec it.
func init() {
	if len(os.Args) < 4 || os.Args[1] != sandbox.HelperSubcommand || os.Args[2] != "--" {
		return
	}
	p, err := sandbox.DecodePolicy(os.Getenv(sandbox.PolicyEnvVar))
	if err == nil {
		err = os.Unsetenv(sandbox.PolicyEnvVar)
	}
	if err == nil {
		err = sandbox.InstallAndExec(p, os.Args[3], os.Args[3:], os.Environ())
	}
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func TestShellOperatorPolicyExecutesConfined(t *testing.T) {
	t.Setenv("LOBSLAW_SHELL_ALLOWED", "visible")
	t.Setenv("LOBSLAW_SHELL_SECRET", "hidden")
	registry := NewRegistry()
	builtins := NewBuiltins()
	if err := RegisterShellBuiltin(builtins, registry); err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy("shell_command", &sandbox.Policy{
		EnvWhitelist: []string{"LOBSLAW_SHELL_ALLOWED"},
		Seccomp:      sandbox.SeccompPolicy{Deny: []string{"getppid"}},
	})
	handler, _ := builtins.Get("shell_command")
	out, _, err := handler(context.Background(), map[string]string{
		"command": `printf '%s:%s' "$LOBSLAW_SHELL_ALLOWED" "${LOBSLAW_SHELL_SECRET:-unset}"; echo device >/dev/null`,
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || result.Stdout != "visible:unset" {
		t.Fatalf("confined shell: %+v", result)
	}
	registry.SetPolicy("shell_command", &sandbox.Policy{Seccomp: sandbox.SeccompPolicy{Deny: []string{"execve"}}})
	out, _, err = handler(context.Background(), map[string]string{"command": "echo must-not-run"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 || result.Stdout != "" {
		t.Fatalf("updated seccomp policy was ignored: %+v", result)
	}
}
