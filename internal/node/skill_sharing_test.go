package node

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/sharing"
	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestShareRPCPreviewInstallActivate(t *testing.T) {
	svc := skillSvc(t, skills.SigningOff, nil)
	a, err := sharing.Build(sharing.Package{Format: sharing.Format, Schema: 1, Name: "tidy", Version: "1.2.3", Manifest: []byte(plainManifest("print('hi')")), Files: map[string][]byte{"handler.py": []byte("print('hi')")}})
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
