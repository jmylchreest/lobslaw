package tools

import (
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// agentEnv stacks a real Registry, a permissive policy engine and a
// scripted provider so a tool can be exercised through the full
// executor path. compute has an equivalent for its own tests, but its
// fixtures are unexported and this package sits above it — and using
// the real Registry here is the more faithful test anyway.
type agentEnv struct {
	reg      *Registry
	policy   *policy.Engine
	store    *memory.Store
	executor *compute.Executor
	mock     *compute.MockProvider
	agent    *compute.Agent
}

func newAgentEnv(t *testing.T, responses ...compute.MockResponse) *agentEnv {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(t.TempDir(), "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	seedAllowAll(t, store)

	eng := policy.NewEngine(store, nil)
	reg := NewRegistry()
	exec := compute.NewExecutor(reg, eng, nil, compute.ExecutorConfig{}, nil)
	mock := compute.NewMockProvider(responses...)
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: mock, Executor: exec})
	if err != nil {
		t.Fatal(err)
	}
	return &agentEnv{reg: reg, policy: eng, store: store, executor: exec, mock: mock, agent: agent}
}

func seedAllowAll(t *testing.T, store *memory.Store) {
	t.Helper()
	rule := &lobslawv1.PolicyRule{
		Id: "allow-all", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 1,
	}
	raw, err := proto.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketPolicyRules, rule.Id, raw); err != nil {
		t.Fatal(err)
	}
}
