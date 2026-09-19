package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/computer"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type browserRecorder struct {
	calls          int
	owner, project string
}

func browserTestScope(ctx context.Context) (string, string, error) {
	id, ok := turn.IdentityFrom(ctx)
	if !ok || (id.Principal.IsBot() && id.BotID != "worker") {
		return "", "", computer.ErrForbidden
	}
	return "user:alice", "project", nil
}

func (b *browserRecorder) RunStep(_ context.Context, owner, project string, _ computer.RoutineStep) (computer.Result, error) {
	if owner != "user:alice" || project != "project" {
		return computer.Result{}, computer.ErrForbidden
	}
	b.calls++
	b.owner = owner
	b.project = project
	return computer.Result{Screenshot: "never in model output", Observation: &computer.Observation{Text: "Visible page"}}, nil
}

func TestBrowserToolsRequireTrustedMatchingProjectContext(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	runtime := &browserRecorder{}
	if err := RegisterBrowserBuiltins(b, runtime, browserTestScope); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("browser_capture")
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		args    map[string]string
		allowed bool
	}{
		{"no identity", t.Context(), nil, false},
		{"channel string is not authority", turn.WithIdentity(t.Context(), turn.Identity{Principal: identity.Principal("user:alice"), Channel: "project", ChannelID: "project"}), nil, false},
		{"cross owner", turn.WithIdentity(t.Context(), turn.Identity{Principal: identity.Principal("user:bob"), Channel: "workforce", ChannelID: "project"}), nil, false},
		{"forged JSON scope", turn.WithIdentity(t.Context(), turn.Identity{Principal: identity.Principal("user:alice"), Channel: "workforce", ChannelID: "project"}), map[string]string{"project_id": "other"}, false},
		{"owned bot", turn.WithIdentity(t.Context(), turn.Identity{Principal: identity.Bot("worker"), BotOwner: identity.Principal("user:alice"), BotID: "worker", Channel: "workforce", ChannelID: "project"}), nil, true},
		{"nested bot cannot borrow claim", turn.WithIdentity(t.Context(), turn.Identity{Principal: identity.Bot("outsider"), BotOwner: identity.User("alice"), BotID: "outsider", Channel: "workforce", ChannelID: "project"}), nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, err := fn(tc.ctx, tc.args)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
			if strings.Contains(string(body), "never in model output") {
				t.Fatal("screenshot leaked into transcript")
			}
		})
	}
	if runtime.calls != 1 || runtime.owner != "user:alice" || runtime.project != "project" {
		t.Fatalf("scope mismatch: %+v", runtime)
	}
	for _, name := range []string{"browser_takeover", "browser_release", "browser_record"} {
		if _, ok := b.Get(name); ok {
			t.Fatalf("agent can control owner fence via %s", name)
		}
	}
}

func browserAgent(t *testing.T, service Browser, allowedTools []string, responses ...compute.MockResponse) (*compute.Agent, *agentEnv) {
	t.Helper()
	env := newAgentEnv(t, responses...)
	b := NewBuiltins()
	if err := RegisterBrowserBuiltins(b, service, browserTestScope); err != nil {
		t.Fatal(err)
	}
	for _, def := range BrowserToolDefs() {
		if err := env.reg.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	env.executor.SetBuiltins(b)
	agent, err := compute.NewAgent(compute.AgentConfig{Provider: env.mock, Executor: env.executor, Registry: env.reg, Bots: messagingProfiles{"worker": {ID: "worker", Owner: "user:alice", Tools: allowedTools}}})
	if err != nil {
		t.Fatal(err)
	}
	return agent, env
}

func TestBrowserInvocationUsesAgentPolicyAndBotAllowlist(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		deny  bool
		tools []string
		calls int
	}{
		{"allowed", false, []string{"browser_capture"}, 1},
		{"policy denied", true, []string{"browser_capture"}, 0},
		{"bot allowlist denied", false, []string{"memory_search"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &browserRecorder{}
			agent, env := browserAgent(t, service, tc.tools, compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "capture", Name: "browser_capture", Arguments: `{}`}}}, compute.MockResponse{Content: "done"})
			if tc.deny {
				raw, err := proto.Marshal(&lobslawv1.PolicyRule{Id: "deny-browser", Subject: "*", Action: "tool:exec", Resource: "browser_capture", Effect: "deny", Priority: 100})
				if err != nil {
					t.Fatal(err)
				}
				if err := env.store.Put(memory.BucketPolicyRules, "deny-browser", raw); err != nil {
					t.Fatal(err)
				}
			}
			response, err := agent.Run(t.Context(), turn.Request{Message: "Inspect the project browser", Claims: &types.Claims{UserID: "alice"}, Principal: identity.Bot("worker"), BotID: "worker", Channel: "workforce", ChannelID: "project"})
			if err != nil {
				t.Fatal(err)
			}
			if service.calls != tc.calls {
				t.Fatalf("browser invocations=%d want %d", service.calls, tc.calls)
			}
			if service.calls > 0 {
				raw, _ := json.Marshal(response)
				if !strings.Contains(string(raw), "Visible page") || strings.Contains(string(raw), "never in model output") {
					t.Fatalf("unsafe/missing tool observation: %s", raw)
				}
			}
		})
	}
}
