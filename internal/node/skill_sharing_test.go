package node

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/clawhub"
	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/sharing"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/internal/tools"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/config"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestShareRPCPreviewInstallActivate(t *testing.T) {
	svc := skillSvc(t, skills.SigningOff, nil)
	a, err := sharing.Build(sharing.Package{Format: sharing.Format, Schema: 1, Name: "tidy", Version: "1.2.3", Manifest: []byte(plainManifest("print('hi')")), Files: map[string][]byte{"handler.py": []byte("print('hi')")}, Schedules: []sharing.Schedule{{Key: "daily", Name: "Daily tidy", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "tidy", NotifyOn: "never"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := &lobslawv1.InstallShareRequest{Artifact: a.Bytes(), Owner: "user:alice"}
	if _, err := svc.InstallShare(ctx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing authorization accepted: %v", err)
	}
	svc.authorizeShare = func(_ context.Context, action, owner string) (string, error) {
		if owner != "user:alice" {
			return "", status.Error(codes.PermissionDenied, "wrong owner")
		}
		return "user:alice", nil
	}
	preview, err := svc.InstallShare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var plan memory.SharePlan
	if err := json.Unmarshal(preview.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.Get("tidy", "1.2.3"); err == nil {
		t.Fatal("preview wrote skill")
	}
	req.Apply = true
	req.ExpectedPlan = plan.Digest
	if _, err := svc.InstallShare(ctx, req); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.store.Get("tidy", "1.2.3")
	if err != nil || rec.Active {
		t.Fatal("install should stage", err)
	}
	activate := &lobslawv1.ActivateShareRequest{InstallationId: plan.InstallationID, Owner: "user:alice"}
	activationPreview, err := svc.ActivateShare(ctx, activate)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(activationPreview.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	var disclosure struct {
		Authority string `json:"schedule_authority"`
	}
	if err := json.Unmarshal(activationPreview.PlanJson, &disclosure); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"user:alice", "current permissions", "any tools", "scheduler scope", "confirmation"} {
		if !strings.Contains(disclosure.Authority, text) {
			t.Fatalf("activation omits %q: %s", text, activationPreview.PlanJson)
		}
	}
	activate.Apply = true
	activate.ExpectedPlan = plan.Digest
	if _, err := svc.ActivateShare(ctx, activate); err != nil {
		t.Fatal(err)
	}
	rec, err = svc.store.Get("tidy", "1.2.3")
	if err != nil || !rec.Active {
		t.Fatal("activation failed", err)
	}
	activate.Owner = "user:bob"
	if _, err := svc.ActivateShare(ctx, activate); status.Code(err) != codes.PermissionDenied {
		t.Fatal("cross-owner activation accepted", err)
	}
}

func TestClawhubArtifactUsesSharedRaftInstallAndActivation(t *testing.T) {
	var raw bytes.Buffer
	zw := zip.NewWriter(&raw)
	w, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("---\nname: demo\n---\nUse this skill for demo tasks.\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(raw.Bytes()) }))
	defer srv.Close()
	source, err := clawhub.NewShareSource(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := source.Fetch(context.Background(), "clawhub:demo")
	if err != nil {
		t.Fatal(err)
	}
	svc := skillSvc(t, skills.SigningOff, nil)
	svc.authorizeShare = func(context.Context, string, string) (string, error) { return "user:alice", nil }
	ctx := context.Background()
	req := &lobslawv1.InstallShareRequest{Artifact: artifact.Bytes(), Owner: "user:alice"}
	preview, err := svc.InstallShare(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var plan memory.SharePlan
	if err := json.Unmarshal(preview.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	req.Apply = true
	req.ExpectedPlan = plan.Digest
	if _, err := svc.InstallShare(ctx, req); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.store.Get("demo", "0.0.0")
	if err != nil || rec.Active {
		t.Fatal("ClawHub install didn't stage", err)
	}
	activate := &lobslawv1.ActivateShareRequest{InstallationId: plan.InstallationID, Owner: "user:alice"}
	activation, err := svc.ActivateShare(ctx, activate)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(activation.PlanJson, &plan); err != nil {
		t.Fatal(err)
	}
	activate.Apply = true
	activate.ExpectedPlan = plan.Digest
	if _, err := svc.ActivateShare(ctx, activate); err != nil {
		t.Fatal(err)
	}
	rec, err = svc.store.Get("demo", "0.0.0")
	if err != nil || !rec.Active {
		t.Fatal("ClawHub activation failed", err)
	}
}

func TestShareRejectsManifestIdentityMismatch(t *testing.T) {
	svc := skillSvc(t, skills.SigningOff, nil)
	svc.authorizeShare = func(context.Context, string, string) (string, error) { return "user:alice", nil }
	a, err := sharing.Build(sharing.Package{Format: sharing.Format, Schema: 1, Name: "imposter", Version: "1.2.3", Manifest: []byte(plainManifest("print('hi')")), Files: map[string][]byte{"handler.py": []byte("print('hi')")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.InstallShare(context.Background(), &lobslawv1.InstallShareRequest{Artifact: a.Bytes(), Owner: "user:alice"}); err == nil {
		t.Fatal("mismatched manifest accepted")
	}
}

func TestClawhubManifestSignatureSurvivesConversionAndIsRequiredAtInstall(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	const originalHandler = "print('hi')"
	originalManifest := plainManifest(originalHandler)
	signature := ed25519.Sign(priv, []byte(originalManifest))
	for _, tc := range []struct {
		name, manifest, handler string
		tampered                bool
	}{
		{"valid", originalManifest, originalHandler, false},
		{"manifest changed", originalManifest + "description: changed after signing\n", originalHandler, true},
		// Updating the declared digest defeats a digest-only check, but must
		// not bypass verification of the original publisher's signature.
		{"handler and digest changed", plainManifest("print('changed')"), "print('changed')", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw bytes.Buffer
			zw := zip.NewWriter(&raw)
			for name, content := range map[string][]byte{
				"manifest.yaml": []byte(tc.manifest), "manifest.yaml.sig": signature,
				"handler.py": []byte(tc.handler),
			} {
				w, err := zw.Create(name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write(content); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(raw.Bytes()) }))
			defer srv.Close()
			source, err := clawhub.NewShareSource(srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			artifact, err := source.Fetch(t.Context(), "clawhub:tidy")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(artifact.Package().ManifestSignature, signature) {
				t.Fatal("conversion discarded or changed the publisher signature")
			}
			verifier := skills.NewVerifier()
			if err := verifier.AddKey("publisher", pub); err != nil {
				t.Fatal(err)
			}
			// A present signature is mandatory even with destination signing off.
			svc := skillSvc(t, skills.SigningOff, verifier)
			svc.authorizeShare = func(context.Context, string, string) (string, error) { return "user:alice", nil }
			req := &lobslawv1.InstallShareRequest{Artifact: artifact.Bytes(), Owner: "user:alice"}
			preview, err := svc.InstallShare(t.Context(), req)
			if tc.tampered {
				if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "signature") {
					t.Fatalf("tampered signature was not rejected: %v", err)
				}
				if _, err := svc.store.Get("tidy", "1.2.3"); !errors.Is(err, memory.ErrSkillNotFound) {
					t.Fatalf("rejected bundle wrote a skill: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var plan memory.SharePlan
			if err := json.Unmarshal(preview.PlanJson, &plan); err != nil {
				t.Fatal(err)
			}
			req.Apply, req.ExpectedPlan = true, plan.Digest
			if _, err := svc.InstallShare(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			rec, err := svc.store.Get("tidy", "1.2.3")
			if err != nil || rec.Active || !bytes.Equal(rec.ManifestSig, signature) {
				t.Fatalf("valid signed bundle did not stage intact: record=%v err=%v", rec, err)
			}
		})
	}
}

func TestShareRuntimeRejectsMissingApprovalBeforeAgentStarts(t *testing.T) {
	n := &Node{}
	for _, task := range []*lobslawv1.ScheduledTaskRecord{
		{Id: "normal-id", Params: map[string]string{"share_installation": "missing", "prompt": "run"}},
		{Id: "share-stripped-params:daily", Params: map[string]string{"prompt": "run"}},
	} {
		if err := n.runTaskAsAgentTurn(context.Background(), task); err == nil {
			t.Fatal("unapproved shared task reached agent")
		}
	}
}

// Exercise delegation through the real approval store, agent loop and policy
// executor. The unrelated builtin proves authority extends beyond the skill.
func TestSharedScheduleUsesCurrentOwnerPermissions(t *testing.T) {
	svc, n := skillSvcNode(t, skills.SigningOff, nil)
	n.log = slog.Default()
	artifact, err := sharing.Build(sharing.Package{Format: sharing.Format, Schema: 1, Name: "tidy", Version: "1.2.3", Manifest: []byte(plainManifest("print('hi')")), Files: map[string][]byte{"handler.py": []byte("print('hi')")}, Schedules: []sharing.Schedule{{Key: "daily", Name: "Daily tidy", Cron: "0 9 * * *", Timezone: "UTC", Prompt: "use the unrelated tool", NotifyOn: "never"}}})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := svc.sharing.PlanInstall(artifact, "user:alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.sharing.Apply(t.Context(), plan, plan.Digest); err != nil {
		t.Fatal(err)
	}
	activation, err := svc.sharing.PlanActivate(plan.InstallationID, "user:alice", "operator")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.sharing.Apply(t.Context(), activation, activation.Digest); err != nil {
		t.Fatal(err)
	}
	raw, err := n.store.Get(memory.BucketScheduledTasks, plan.ScheduleIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	task := new(lobslawv1.ScheduledTaskRecord)
	if err := proto.Unmarshal(raw, task); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for path, data := range map[string][]byte{"manifest.yaml": artifact.Package().Manifest, "handler.py": artifact.Package().Files["handler.py"]} {
		if err := os.WriteFile(filepath.Join(dir, path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	skill, err := skills.ParseWithPolicy(dir, skills.SigningOff, nil)
	if err != nil {
		t.Fatal(err)
	}
	n.skillRegistry = skills.NewRegistry(n.log)
	n.skillRegistry.Put(skill)
	registry := tools.NewRegistry()
	if err := registry.Register(&types.ToolDef{Name: "unrelated", Path: "builtin:unrelated", RiskTier: types.RiskReversible}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	builtins := tools.NewBuiltins()
	if err := builtins.Register("unrelated", func(ctx context.Context, _ map[string]string) ([]byte, int, error) {
		caller, ok := turn.IdentityFrom(ctx)
		if !ok || caller.UserID != "alice" || caller.Scope != "scheduler" {
			t.Errorf("wrong delegated identity: %+v", caller)
		}
		calls++
		return []byte("done"), 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	executor := compute.NewExecutor(registry, policy.NewEngine(n.store, n.log), nil, compute.ExecutorConfig{}, n.log)
	executor.SetBuiltins(builtins)
	for _, tc := range []struct {
		name          string
		roles         []string
		effect, scope string
		wantCalls     int
		confirmation  bool
	}{
		{"owner grant", []string{"reader"}, "allow", "scheduler", 1, false},
		{"revoked role", nil, "allow", "scheduler", 0, false},
		{"new grant", []string{"reader"}, "allow", "scheduler", 1, false},
		{"scope restriction", []string{"reader"}, "allow", "interactive", 0, false},
		{"explicit deny", []string{"reader"}, "deny", "scheduler", 0, false},
		{"confirmation required", []string{"reader"}, "require_confirmation", "scheduler", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n.cfg.Users = []config.UserConfig{{ID: "alice", Roles: tc.roles}, {ID: "operator", Roles: []string{"reader"}}}
			seedRule(t, n.store, &lobslawv1.PolicyRule{Id: "delegation", Subject: "role:reader", Action: "*", Resource: "*", Effect: tc.effect, Scope: tc.scope, Priority: 50})
			provider := compute.NewMockProvider(compute.MockResponse{ToolCalls: []compute.ToolCall{{ID: "call", Name: "unrelated", Arguments: "{}"}}}, compute.MockResponse{Content: "done"})
			n.agent, err = compute.NewAgent(compute.AgentConfig{Provider: provider, Executor: executor, Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			calls = 0
			err = n.runTaskAsAgentTurn(t.Context(), task)
			if tc.confirmation {
				if err == nil || !strings.Contains(err.Error(), "requires interactive approval") {
					t.Fatalf("confirmation did not block completion: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("tool executed %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}
