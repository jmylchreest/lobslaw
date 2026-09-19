package node

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type learnedReviews struct{ n *Node }

func (n *Node) learnedReviews() gateway.LearnedReviews {
	if n.selfTaught == nil || n.policyEngine == nil {
		return nil
	}
	return learnedReviews{n: n}
}

func (r learnedReviews) authorise(ctx context.Context, claims *types.Claims) (string, error) {
	if claims == nil || claims.UserID == "" || !commandAuthorizer(r).AllowsCommand(ctx, claims, "learned") {
		return "", fmt.Errorf("not authorised to review learned skills")
	}
	return "user:" + claims.UserID, nil
}

func (r learnedReviews) List(ctx context.Context, claims *types.Claims) ([]gateway.LearnedReview, error) {
	owner, err := r.authorise(ctx, claims)
	if err != nil {
		return nil, err
	}
	rows, err := r.n.selfTaught.List(memory.SelfTaughtQuery{Owner: owner})
	if err != nil {
		return nil, err
	}
	var out []gateway.LearnedReview
	for _, rec := range rows {
		if rec.Owner == owner && (rec.State == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED || rec.Pending != nil) {
			out = append(out, learnedReviewView(rec))
		}
	}
	return out, nil
}

func (r learnedReviews) Get(ctx context.Context, claims *types.Claims, id string) (gateway.LearnedReview, error) {
	owner, err := r.authorise(ctx, claims)
	if err != nil {
		return gateway.LearnedReview{}, err
	}
	rec, err := r.n.selfTaught.Get(id)
	if err != nil || rec.Owner != owner {
		return gateway.LearnedReview{}, fmt.Errorf("proposal not found for this user")
	}
	if rec.State != lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_PROPOSED && rec.Pending == nil {
		return gateway.LearnedReview{}, fmt.Errorf("this proposal has already been decided")
	}
	return learnedReviewView(rec), nil
}

func (r learnedReviews) Decide(ctx context.Context, claims *types.Claims, id string, revision uint64, digest string, approve bool) (string, error) {
	owner, err := r.authorise(ctx, claims)
	if err != nil {
		return "", err
	}
	rec, err := r.n.selfTaught.DecideReviewed(ctx, id, revision, digest, owner, approve)
	if err != nil {
		return "", err
	}
	if !approve {
		if rec.State == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ARCHIVED {
			return "Proposal denied and archived. It will not be activated.", nil
		}
		return "Amendment denied. The existing approved version is unchanged.", nil
	}
	if rec.Kind != lobslawv1.SelfTaughtKind_SELF_TAUGHT_KIND_SKILL {
		return "Approval recorded. The learned instruction is active.", nil
	}
	if r.n.materialiser == nil || r.n.skillRegistry == nil {
		return "Approval recorded. Skill activation is pending on a compute node; this node has no skill materialiser.", nil
	}
	if err := r.n.materialiseOnce(); err != nil {
		return "Approval recorded, but local skill activation failed: " + err.Error(), nil
	}
	installed, err := r.n.skillRegistry.Get(rec.Name)
	if err != nil || !reviewedSkillInstalled(installed, rec) {
		return "Approval recorded, but this skill is not active on this node. Materialisation failed or a higher-trust skill owns the name; an operator should check the skill registry and logs.", nil
	}
	return "Approval recorded. The reviewed skill is installed and active on this node.", nil
}

func learnedReviewView(rec *lobslawv1.SelfTaughtRecord) gateway.LearnedReview {
	out := gateway.LearnedReview{ID: rec.Id, Name: rec.Name, Description: rec.Description, Body: rec.Body, Files: rec.Files, Revision: rec.Revision, Digest: memory.SelfTaughtReviewDigest(rec), TurnID: rec.TurnId, Active: rec.State == lobslawv1.SelfTaughtState_SELF_TAUGHT_STATE_ACTIVE}
	if p := rec.Pending; p != nil {
		out.Pending = &gateway.LearnedChange{Description: p.Description, Body: p.Body, Files: p.Files, Rationale: p.Rationale, TurnID: p.TurnId}
	}
	return out
}

func reviewedSkillInstalled(skill *skills.Skill, rec *lobslawv1.SelfTaughtRecord) bool {
	if skill == nil || skill.Tier != skills.TierAgent || skill.BodySHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte(rec.Body))) {
		return false
	}
	for name, body := range rec.Files {
		if !filepath.IsLocal(name) {
			return false
		}
		raw, err := os.ReadFile(filepath.Join(skill.ManifestDir, name))
		if err != nil || string(raw) != body {
			return false
		}
	}
	// Materialised skills also carry the body in the reference index.
	return len(skill.Manifest.References) == len(rec.Files)+1
}
