package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (n *Node) proposeClawhubShare(ctx context.Context, ref string) ([]byte, error) {
	caller, ok := turn.IdentityFrom(ctx)
	owner := caller.Principal.String()
	if !ok || caller.UserID == "" || !strings.HasPrefix(owner, "user:") || len(owner) <= 5 || n.policyEngine == nil {
		return nil, errors.New("clawhub: authenticated user and proposal policy required")
	}
	claims := caller.Claims()
	claims.UserID = caller.Principal.ID()
	decision, err := n.policyEngine.Evaluate(ctx, claims, "skills:share:propose", owner)
	if err != nil || decision.Effect != types.EffectAllow {
		return nil, errors.New("clawhub: an explicit skills:share:propose grant for this owner is required")
	}
	if n.clawhubSource == nil || n.raft == nil || n.store == nil {
		return nil, errors.New("clawhub: source and Raft staging unavailable")
	}
	a, err := n.clawhubSource.Fetch(ctx, ref)
	if err != nil {
		return nil, err
	}
	svc := &skillService{store: n.skillStore, sharing: memory.NewSharingStore(n.raft, n.store), policy: n.skillSigningPolicy, verifier: n.skillVerifier}
	p, err := svc.prepareShareInstall(a, owner, nil)
	if err != nil {
		return nil, err
	}
	// Staging is not approval. No activation capability is given to the model.
	raw, err := svc.shareResult(ctx, p, a, true, p.Digest)
	if err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		InstallationID     string          `json:"installation_id"`
		Owner              string          `json:"owner"`
		ActivationRequired bool            `json:"activation_required"`
		Review             string          `json:"review_instructions"`
		Plan               json.RawMessage `json:"plan"`
	}{p.InstallationID, owner, !p.AlreadyActive, "A human operator must run skills activate-install for this installation and owner, review the returned manifest and plan, then apply its expected-plan digest. Tool execution permissions and binary dependencies require separate operator setup.", raw})
}
