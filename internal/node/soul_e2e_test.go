package node

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func soulTestCreds(t *testing.T, dir, id string) *mtls.NodeCreds {
	t.Helper()
	caPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if _, err := os.Stat(caPath); os.IsNotExist(err) {
		cert, key, err := mtls.GenerateCA(mtls.CAOpts{CommonName: "soul-test"})
		if err != nil {
			t.Fatal(err)
		}
		if err := mtls.WriteCAFiles(caPath, keyPath, cert, key); err != nil {
			t.Fatal(err)
		}
	}
	ca, key, err := mtls.LoadCA(caPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	cert, priv, err := mtls.SignNodeCert(ca, key, mtls.SignOpts{NodeID: id, IPs: []net.IP{net.ParseIP("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	certPath, privPath := filepath.Join(dir, id+".pem"), filepath.Join(dir, id+"-key.pem")
	if err := mtls.WriteNodeFiles(certPath, privPath, cert, priv); err != nil {
		t.Fatal(err)
	}
	creds, err := mtls.LoadNodeCreds(caPath, certPath, privPath)
	if err != nil {
		t.Fatal(err)
	}
	return creds
}

func bootSoulNode(t *testing.T, cfg Config) (*Node, func()) {
	t.Helper()
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("node did not stop")
		}
	}
	t.Cleanup(stop)
	if n.raft != nil {
		if err := n.raft.WaitForLeader(5 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	return n, stop
}

func soulBuiltin(t *testing.T, n *Node, name string, args map[string]string) map[string]any {
	t.Helper()
	fn, ok := n.builtinsRegistry.Get(name)
	if !ok {
		t.Fatalf("%s not registered", name)
	}
	out, code, err := fn(turn.WithIdentity(context.Background(), turn.Identity{UserID: "owner", Scope: "owner"}), args)
	if err != nil || code != 0 {
		t.Fatalf("%s: code=%d err=%v output=%s", name, code, err, out)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func soulTurn(t *testing.T, n *Node) {
	t.Helper()
	budget, _ := compute.NewTurnBudget(compute.BudgetCaps{})
	_, err := n.Agent().RunToolCallLoop(context.Background(), compute.ProcessMessageRequest{
		Message: "What is seven times six?", Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSoulNodeToolReloadAndRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SOUL.md")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte("---\nname: baseline\nemotive_style:\n  directness: 5\n---\n"+body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("Use short sentences.")
	var prompt string
	provider := compute.NewMockProviderFunc(func(req compute.ChatRequest, _ int) (compute.MockResponse, error) {
		for _, m := range req.Messages {
			if m.Role == "system" {
				prompt = m.Content
			}
			if m.Role == "user" && m.Content != "What is seven times six?" {
				t.Errorf("unexpected user message: %q", m.Content)
			}
		}
		return compute.MockResponse{Content: "42", FinishReason: "stop"}, nil
	})
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{NodeID: "soul", Creds: soulTestCreds(t, dir, "soul"), MemoryKey: key,
		Functions:  []types.NodeFunction{types.FunctionMemory, types.FunctionCompute},
		ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "data"), Bootstrap: true,
		SnapshotTarget: "storage:test", SoulPath: path, LLMProvider: provider}
	n, stop := bootSoulNode(t, cfg)
	soulTurn(t, n)
	if !strings.Contains(prompt, "Use short sentences.") || !strings.Contains(prompt, "not a question") {
		t.Fatal("body or configuration framing missing")
	}
	soulBuiltin(t, n, "soul_fragment_add", map[string]string{"text": "Prefers aubergines."})
	soulBuiltin(t, n, "soul_tune", map[string]string{"field": "directness", "delta": "3"})
	soulTurn(t, n)
	if !strings.Contains(prompt, "Prefers aubergines.") || !strings.Contains(prompt, "Lead with the answer.") {
		t.Fatal("successful tool edit did not affect next turn")
	}
	write("Use complete sentences.")
	_, _, errs := n.reloadSections(context.Background(), []string{ReloadSoul})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	got := soulBuiltin(t, n, "soul_get", nil)
	if got["body"] != "Use complete sentences." || len(got["overrides"].([]any)) != 2 {
		t.Fatalf("stale soul_get: %v", got)
	}
	soulTurn(t, n)
	if !strings.Contains(prompt, "Use complete sentences.") || strings.Contains(prompt, "Use short sentences.") {
		t.Fatal("reload not reflected in prompt")
	}
	if err := os.WriteFile(path, []byte("---\nname: [\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.reloadSoul(); err == nil {
		t.Fatal("invalid reload succeeded")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := n.reloadSoul(); err == nil {
		t.Fatal("missing reload succeeded")
	}
	if n.Soul().Body != "Use complete sentences." {
		t.Fatal("bad/missing file discarded valid soul")
	}
	write("Use complete sentences.")
	soulBuiltin(t, n, "soul_reset", map[string]string{"field": "directness"})
	if n.Soul().Config.EmotiveStyle.Directness != 5 {
		t.Fatal("reset did not inherit file")
	}
	cfg.ListenAddr = n.ListenAddr()
	stop()
	n, _ = bootSoulNode(t, cfg)
	soulTurn(t, n)
	if !strings.Contains(prompt, "Prefers aubergines.") {
		t.Fatal("persisted fragment lost after restart")
	}
	soulBuiltin(t, n, "soul_history_rollback", map[string]string{"steps": "1"})
	if n.Soul().Config.EmotiveStyle.Directness != 8 {
		t.Fatal("rollback history lost after restart")
	}
}

func TestSoulComputeOnlyUsesCluster(t *testing.T) {
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	leader, _ := bootSoulNode(t, Config{NodeID: "soul-memory", Creds: soulTestCreds(t, dir, "soul-memory"),
		Functions: []types.NodeFunction{types.FunctionMemory}, MemoryKey: key,
		ListenAddr: "127.0.0.1:0", DataDir: filepath.Join(dir, "memory"), Bootstrap: true, SnapshotTarget: "storage:test"})

	if _, err := leader.Policy().AddRule(context.Background(), &lobslawv1.AddRuleRequest{Rule: &lobslawv1.PolicyRule{
		Id: "soul-owner", Subject: "scope:owner", Action: "tool:exec", Resource: "soul_*", Effect: "allow", Priority: 20,
	}}); err != nil {
		t.Fatal(err)
	}
	var prompt string
	emitTune := false
	provider := compute.NewMockProviderFunc(func(req compute.ChatRequest, _ int) (compute.MockResponse, error) {
		prompt = req.Messages[0].Content
		if emitTune {
			emitTune = false
			return compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "tune-emoji", Name: "soul_tune", Arguments: `{"field":"emoji_usage","value":"generous"}`}}}, nil
		}
		return compute.MockResponse{Content: "42", FinishReason: "stop"}, nil
	})
	cfg := Config{NodeID: "soul-compute", Creds: soulTestCreds(t, dir, "soul-compute"),
		Functions: []types.NodeFunction{types.FunctionCompute}, ListenAddr: "127.0.0.1:0",
		SeedNodes: []string{leader.ListenAddr()}, LLMProvider: provider}
	n, stop := bootSoulNode(t, cfg)

	if err := n.executor.CheckPolicy(context.Background(), &types.Claims{UserID: "guest", Scope: "guest"}, "tool:exec", "soul_fragment_add"); err == nil {
		t.Fatal("compute-only node bypassed soul policy")
	}

	soulBuiltin(t, n, "soul_fragment_add", map[string]string{"text": "Enjoys strong tea."})
	soulTurn(t, n)
	if !strings.Contains(prompt, "Enjoys strong tea.") {
		t.Fatal("remote edit absent from prompt")
	}
	state, err := leader.soulSnapshot(context.Background())
	if err != nil || len(state.Config.Fragments) != 1 {
		t.Fatalf("remote edit not in cluster: %v", err)
	}
	stop()
	n, _ = bootSoulNode(t, cfg)
	soulTurn(t, n)
	if !strings.Contains(prompt, "Enjoys strong tea.") {
		t.Fatal("compute-only restart lost overlay")
	}
	soulBuiltin(t, n, "soul_history_rollback", nil)
	soulTurn(t, n)
	if strings.Contains(prompt, "Enjoys strong tea.") {
		t.Fatal("remote rollback did not restore inheritance")
	}
	// Drive the actual model -> executor -> builtin -> cluster path. The
	// in-flight request keeps its snapshot; only the next turn sees the edit.
	emitTune = true
	budget, _ := compute.NewTurnBudget(compute.BudgetCaps{})
	if _, err := n.Agent().RunToolCallLoop(context.Background(), compute.ProcessMessageRequest{
		Message: "Use more emoji", Budget: budget, Claims: &types.Claims{UserID: "owner", Scope: "owner"},
	}); err != nil {
		t.Fatal(err)
	}
	if n.Soul().Config.EmotiveStyle.EmojiUsage != "generous" {
		t.Fatal("agent tool call did not persist tuning")
	}
	if !strings.Contains(prompt, "Do not use emoji.") || strings.Contains(prompt, "Emoji are welcome where they carry tone.") {
		t.Fatal("in-flight turn did not retain its original soul snapshot")
	}
	soulTurn(t, n)
	if !strings.Contains(prompt, "Emoji are welcome where they carry tone.") || strings.Contains(prompt, "Do not use emoji.") {
		t.Fatal("next turn did not pick up model-initiated tuning")
	}

}

func TestStandaloneSoulCannotReportVolatileEditAsDurable(t *testing.T) {
	dir := t.TempDir()
	n, _ := bootSoulNode(t, Config{NodeID: "standalone-soul", Creds: soulTestCreds(t, dir, "standalone-soul"),
		Functions: []types.NodeFunction{types.FunctionCompute}, ListenAddr: "127.0.0.1:0",
		LLMProvider: compute.NewMockProvider(compute.MockResponse{Content: "ok"}),
	})
	if _, err := n.soulAdjuster.SetName(context.Background(), "volatile"); err == nil {
		t.Fatal("standalone edit falsely reported success")
	}
	if n.Soul().Config.Name != "assistant" {
		t.Fatal("failed edit changed live state")
	}
}
