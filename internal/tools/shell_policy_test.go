package tools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

func TestShellOperatorPolicy(t *testing.T) {
	operator := &sandbox.Policy{
		Mounts:  []sandbox.PolicyMount{{Path: "/operator", Read: true}},
		Seccomp: sandbox.SeccompPolicy{Deny: []string{"getppid"}},
	}
	got, err := buildShellPolicy(operator)
	if err != nil {
		t.Fatal(err)
	}
	if !got.NoNewPrivs || !got.AllowsPath("/operator/file", sandbox.AccessR) || got.AllowsPath("/operator/file", sandbox.AccessW) {
		t.Fatalf("operator path not enforced: %+v", got)
	}
	if !got.AllowsPath("/bin/sh", sandbox.AccessRX) {
		t.Fatal("shell runtime floor missing")
	}
	for _, name := range append(sandbox.DefaultSeccompPolicy.Deny, "getppid") {
		found := false
		for _, deny := range got.Seccomp.Deny {
			if deny == name {
				found = true
			}
		}
		if !found {
			t.Errorf("missing syscall denial %q", name)
		}
	}
	got.Mounts[0].Path = "/changed"
	got.Seccomp.Deny[0] = "changed"
	if operator.Mounts[0].Path != "/operator" || !reflect.DeepEqual(operator.Seccomp.Deny, []string{"getppid"}) {
		t.Fatal("mutated shared registry policy")
	}
}

func TestShellPolicyReloadAndRemoval(t *testing.T) {
	registry := NewRegistry()
	builtins := NewBuiltins()
	if err := RegisterShellBuiltin(builtins, registry); err != nil {
		t.Fatal(err)
	}
	handler, ok := builtins.Get("shell_command")
	if !ok {
		t.Fatal("missing shell handler")
	}
	for _, p := range []*sandbox.Policy{{CPUQuota: 1}, {MemoryLimitMB: 1}, {NetworkAllowCIDR: []string{"127.0.0.0/8"}}, {DangerousCmdsDeny: []string{"echo"}}} {
		registry.SetPolicy("shell_command", p)
		if _, _, err := handler(context.Background(), map[string]string{"command": "echo policy"}); err == nil {
			t.Fatalf("unsupported policy silently ignored: %+v", p)
		}
	}
	registry.SetPolicy("shell_command", nil)
	out, _, err := handler(context.Background(), map[string]string{"command": "echo policy"})
	if err != nil || !strings.Contains(string(out), "policy") {
		t.Fatalf("deleted policy retained: %s, %v", out, err)
	}
}
