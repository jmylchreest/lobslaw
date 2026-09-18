//go:build linux

package sandbox_test

import (
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/internal/tools"
)

func checkChildPolicyDefaults(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	for _, entry := range cmd.Env {
		if raw, ok := strings.CutPrefix(entry, sandbox.PolicyEnvVar+"="); ok {
			child, err := sandbox.DecodePolicy(raw)
			if err != nil {
				t.Error(err)
				return
			}
			if !child.NoNewPrivs || !reflect.DeepEqual(child.Seccomp, sandbox.DefaultSeccompPolicy) {
				t.Errorf("child did not receive defaults: %+v", child)
			}
			return
		}
	}
	t.Error("no child sandbox policy")
}

func TestApplyPreservesCallerPolicy(t *testing.T) {
	p := &sandbox.Policy{Mounts: []sandbox.PolicyMount{{Path: "/tmp", Read: true}}}
	before, err := sandbox.EncodePolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/true")
	if err := sandbox.Apply(cmd, p); err != nil {
		t.Fatal(err)
	}
	checkChildPolicyDefaults(t, cmd)
	after, err := sandbox.EncodePolicy(p)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("Apply mutated caller NoNewPrivs/Seccomp")
	}
}

// Each goroutine owns its command but shares the registry policy, as the
// subprocess executor does. Apply only prepares commands; none are started.
func TestApplySharedRegistryPolicyConcurrently(t *testing.T) {
	registry := tools.NewRegistry()
	for round := 0; round < 30; round++ {
		p := &sandbox.Policy{Mounts: []sandbox.PolicyMount{{Path: "/tmp", Read: true}}}
		registry.SetPolicy("probe", p)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for worker := 0; worker < 16; worker++ {
			wg.Go(func() {
				<-start
				cmd := exec.Command("/bin/true")
				if err := sandbox.Apply(cmd, registry.PolicyFor("probe")); err != nil {
					t.Error(err)
					return
				}
				checkChildPolicyDefaults(t, cmd)
			})
		}
		close(start)
		wg.Wait()
	}
}
