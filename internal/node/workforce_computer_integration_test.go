package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/raft"

	"github.com/jmylchreest/lobslaw/internal/computer"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/internal/workforce"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 3 && os.Args[1] == sandbox.HelperSubcommand {
		p, err := sandbox.DecodePolicy(os.Getenv(sandbox.PolicyEnvVar))
		if err == nil {
			_ = os.Unsetenv(sandbox.PolicyEnvVar)
			err = sandbox.InstallAndExec(p, os.Args[3], os.Args[3:], os.Environ())
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// This test joins both workstreams: real Raft task state, node policy/wiring,
// Chromium, namespace containment and the owner-facing control fence.
func newIntegratedWorkforceComputer(t *testing.T) (*Node, *httptest.Server) {
	t.Helper()
	nodePath := os.Getenv("COMPUTER_TEST_NODE")
	if nodePath == "" {
		t.Skip("set COMPUTER_TEST_NODE/CHROMIUM/PLAYWRIGHT for browser integration")
	}
	ctx := t.Context()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/login" {
			_, _ = fmt.Fprint(w, `<label>Password<input id="password" type="password"></label>`)
			return
		}
		if r.URL.Path == "/hostile" {
			_, _ = fmt.Fprint(w, `<p>Public page</p><p id="probe">Nothing leaked</p><script>const original=String.prototype.replaceAll;String.prototype.replaceAll=function(pattern,...args){if(pattern==='private-secret-value'){document.getElementById('probe').textContent='LEAKED TO PAGE'}return original.call(this,pattern,...args)}</script>`)
			return
		}
		_, _ = fmt.Fprint(w, `<label>Search<input id="search" type="search"></label><button id="submit" onclick="document.getElementById('result').textContent='Found '+document.getElementById('search').value">Search</button><p id="result"></p>`)
	}))
	t.Cleanup(web.Close)
	proxy, err := egress.NewSmokescreenProvider(egress.SmokescreenConfig{
		UDSPath: filepath.Join(t.TempDir(), "egress.sock"),
		ACL:     egress.Rules{Roles: map[string][]string{"computer": {"127.0.0.1", "localhost"}}}, AllowRanges: []string{"127.0.0.0/8"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Stop(context.Background()) })
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	_, transport := raft.NewInmemTransport("workforce-computer")
	raftNode, err := memory.NewRaft(memory.RaftConfig{NodeID: "workforce-computer", LocalAddr: "workforce-computer", DataDir: dir, Bootstrap: true, Transport: transport}, memory.NewFSM(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raftNode.Shutdown(); _ = store.Close() })
	if err = raftNode.WaitForLeader(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	n := &Node{raft: raftNode, store: store, botSvc: memory.NewBotService(raftNode, store), egressProvider: proxy, policyEngine: policy.NewEngine(store, nil), toolRegistry: tools.NewRegistry(), builtinsRegistry: tools.NewBuiltins()}
	n.cfg.Functions = []types.NodeFunction{types.FunctionCompute, types.FunctionComputeTeams}
	n.cfg.Computer = config.ComputerConfig{Enabled: true, Root: filepath.Join(t.TempDir(), "profiles"), Node: nodePath,
		Chromium: os.Getenv("COMPUTER_TEST_CHROMIUM"), Playwright: os.Getenv("COMPUTER_TEST_PLAYWRIGHT"), IP: "/usr/sbin/ip",
		ReadPaths: []string{"/usr", "/lib", "/lib64", "/etc/fonts", "/etc/ssl", "/etc/ld.so.cache", "/proc", "/sys", filepath.Dir(nodePath), filepath.Dir(os.Getenv("COMPUTER_TEST_CHROMIUM")), filepath.Dir(os.Getenv("COMPUTER_TEST_PLAYWRIGHT"))}}
	n.policyEngine.SetDefaults([]types.PolicyRule{{ID: "browser", Subject: "user:alice", Action: "tool:exec", Resource: "browser_*", Effect: types.EffectAllow}})
	if _, err = n.botSvc.Put(ctx, &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0); err != nil {
		t.Fatal(err)
	}
	if err = n.wireWorkforce(); err != nil {
		t.Fatal(err)
	}
	if n.computer == nil {
		t.Fatal("computer adapter was not wired")
	}
	t.Cleanup(func() { _ = n.computer.Close() })
	names := map[string]bool{}
	for _, tool := range n.toolRegistry.LLMTools() {
		names[tool.Name] = true
	}
	if !names["browser_fill"] || !names["workforce_task_create"] {
		t.Fatalf("agent tools not wired: %v", names)
	}
	return n, web
}

func TestWorkforceComputerRunsReviewedRoutine(t *testing.T) {
	n, web := newIntegratedWorkforceComputer(t)
	ctx := t.Context()
	project, err := n.workforce.CreateProject(ctx, "user:alice", workforce.Project{Name: "Search", BotIDs: []string{"worker"}, CoordinatorBotID: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.computer.State(ctx, "user:bob", project.ID); !errors.Is(err, computer.ErrForbidden) {
		t.Fatalf("cross-owner browser: %v", err)
	}
	if _, err = n.computer.State(ctx, project.Owner, "missing"); !errors.Is(err, computer.ErrNotFound) {
		t.Fatalf("missing project mapping: %v", err)
	}
	routine, err := n.workforce.CreateRoutine(ctx, project.Owner, project.ID, workforce.Routine{Name: "Search", Steps: []workforce.RoutineStep{
		{Action: "navigate", URL: web.URL}, {Action: "fill", Selector: "#search", Value: "bread", InputMode: workforce.ReviewedLiteralInput}, {Action: "click", Selector: "#submit"}, {Action: "wait", Selector: "#result"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	claims := &types.Claims{UserID: "alice"}
	routine, err = n.workforce.ActRoutine(ctx, project.Owner, routine.ID, routine.Revision, "approve", workforce.Routine{}, claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.computer.Action(ctx, project.Owner, project.ID, computer.RoutineStep{Action: "takeover"}); err != nil {
		t.Fatal(err)
	}
	task, err := n.workforce.RunRoutine(ctx, project.Owner, routine.ID, claims)
	if err != nil {
		t.Fatal(err)
	}
	n.workforce.WorkOnce(ctx)
	task, _ = n.workforce.GetTask(ctx, project.Owner, task.ID)
	if task.Status != workforce.StatusBlocked || task.ManualStep {
		t.Fatalf("takeover should block without permitting step skip: %+v", task)
	}
	attention, err := n.workforce.Attention(ctx, project.Owner)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range attention {
		found = found || item.Kind == "takeover"
	}
	if !found {
		t.Fatal("takeover missing from Attention")
	}
	if _, err = n.computer.Action(ctx, project.Owner, project.ID, computer.RoutineStep{Action: "release"}); err != nil {
		t.Fatal(err)
	}
	if _, err = n.workforce.ActTask(ctx, project.Owner, task.ID, task.Revision, "retry", "", claims); err != nil {
		t.Fatal(err)
	}
	n.workforce.WorkOnce(ctx)
	task, _ = n.workforce.GetTask(ctx, project.Owner, task.ID)
	if task.Status != workforce.StatusDone || task.Checkpoint != len(routine.Steps) {
		t.Fatalf("routine did not execute: %+v", task)
	}
	result, err := n.computer.Action(ctx, project.Owner, project.ID, computer.RoutineStep{Action: "capture"})
	if err != nil || result.Screenshot == "" {
		t.Fatalf("real browser result missing: %v", err)
	}
	result, err = n.computer.RunStep(ctx, project.Owner, project.ID, computer.RoutineStep{Action: "capture"})
	if err != nil || result.Observation == nil {
		t.Fatalf("agent observation missing: %v", err)
	}
	raw, _ := json.Marshal(result.Observation)
	if !strings.Contains(string(raw), "Found bread") {
		t.Fatalf("browser action did not produce the expected result: %s", raw)
	}
	for _, step := range []computer.RoutineStep{
		{Action: "takeover"}, {Action: "navigate", URL: web.URL + "/login"},
		{Action: "fill", Selector: "#password", Value: "private-secret-value"},
		{Action: "navigate", URL: strings.Replace(web.URL, "127.0.0.1", "localhost", 1) + "/hostile"}, {Action: "release"},
	} {
		if _, err := n.computer.Action(ctx, project.Owner, project.ID, step); err != nil {
			t.Fatal(err)
		}
	}
	result, err = n.computer.RunStep(ctx, project.Owner, project.ID, computer.RoutineStep{Action: "capture"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(result.Observation)
	if strings.Contains(string(raw), "LEAKED TO PAGE") {
		t.Fatal("redaction injected a previous origin's private value into page JavaScript")
	}
}
