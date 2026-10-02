package node

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/sharing"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type proposalSource struct {
	artifact sharing.Artifact
	calls    int
}

func (s *proposalSource) Fetch(context.Context, string) (sharing.Artifact, error) {
	s.calls++
	return s.artifact, nil
}

func TestClawhubProposalRequiresOwnerPolicyAndCannotActivate(t *testing.T) {
	svc, n := skillSvcNode(t, skills.SigningOff, nil)
	n.cfg.Security.ClawhubAutoEmitInstallRules = true
	n.policyEngine = policy.NewEngine(n.store, slog.Default())
	artifact, err := sharing.Build(sharing.Package{Format: sharing.Format, Schema: 1, Name: "tidy", Version: "1.2.3", Manifest: []byte(plainManifest("print('hi')")), Files: map[string][]byte{"handler.py": []byte("print('hi')")}})
	if err != nil {
		t.Fatal(err)
	}
	source := &proposalSource{artifact: artifact}
	n.clawhubSource = source
	ctx := turn.WithIdentity(context.Background(), turn.Identity{UserID: "tg-alice", Principal: identity.User("alice"), Roles: []string{"operator"}})
	if _, err := n.proposeClawhubShare(ctx, "clawhub:tidy"); err == nil {
		t.Fatal("tool access alone granted staging authority")
	}
	if source.calls != 0 {
		t.Fatal("unauthorized proposal fetched content")
	}
	seedRule(t, n.store, &lobslawv1.PolicyRule{Id: "alice-propose", Subject: "user:alice", Action: "skills:share:propose", Resource: "user:alice", Effect: "allow", Priority: 50})
	if _, err := n.proposeClawhubShare(context.Background(), "clawhub:tidy"); err == nil {
		t.Fatal("anonymous staging accepted")
	}
	raw, err := n.proposeClawhubShare(ctx, "clawhub:tidy")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		InstallationID     string `json:"installation_id"`
		ActivationRequired bool   `json:"activation_required"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.InstallationID == "" || !result.ActivationRequired {
		t.Fatal("missing review information")
	}
	rec, err := svc.store.Get("tidy", "1.2.3")
	if err != nil || rec.Active {
		t.Fatal("agent activated a proposal", err)
	}
	if _, err := n.store.Get(memory.BucketPolicyRules, "auto-clawhub-tidy"); err == nil {
		t.Fatal("proposal created an execution policy grant")
	}
	install, err := svc.sharing.Installation(result.InstallationID)
	if err != nil {
		t.Fatal(err)
	}
	if install.Owner != "user:alice" || install.Active || install.ApprovedBy != "" {
		t.Fatal("proposal has wrong ownership or inherited approval")
	}
	svc.authorizeShare = n.authorizeSharing
	if _, err := svc.ActivateShare(ctx, &lobslawv1.ActivateShareRequest{InstallationId: install.Id, Owner: install.Owner}); err == nil {
		t.Fatal("agent turn activated without operator certificate")
	}
	retry, err := n.proposeClawhubShare(ctx, "clawhub:tidy")
	if err != nil {
		t.Fatal(err)
	}
	var again struct {
		InstallationID string `json:"installation_id"`
	}
	if err := json.Unmarshal(retry, &again); err != nil {
		t.Fatal(err)
	}
	if again.InstallationID != install.Id {
		t.Fatal("agent retry duplicated proposal")
	}
}
