package node

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/sharing"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (n *Node) authorizeSharing(ctx context.Context, action, owner string) (string, error) {
	cert := grpcinterceptors.VerifiedPeerCert(ctx)
	if cert == nil || !mtls.IsOperatorCert(cert) || n.policyEngine == nil {
		return "", status.Error(codes.PermissionDenied, "sharing requires an operator certificate and data policy grant")
	}
	actor := "user:" + cert.Subject.CommonName
	if owner == "" {
		owner = actor
	}
	if !strings.HasPrefix(owner, "user:") || len(owner) <= 5 {
		return "", status.Error(codes.InvalidArgument, "owner must be a user principal")
	}
	roles := n.resolveUserRoles(cert.Subject.CommonName)
	if !slices.Contains(roles, "operator") {
		return "", status.Error(codes.PermissionDenied, "sharing requires the configured operator data role")
	}
	decision, err := n.policyEngine.Evaluate(ctx, &types.Claims{UserID: cert.Subject.CommonName, Roles: roles}, action, owner)
	if err != nil || decision.Effect != types.EffectAllow {
		return "", status.Error(codes.PermissionDenied, "sharing action requires an explicit policy grant for this owner")
	}
	return actor, nil
}

func (s *skillService) shareAccess(ctx context.Context, action, owner string) (string, error) {
	if s.authorizeShare == nil {
		return "", status.Error(codes.PermissionDenied, "sharing authorization unavailable")
	}
	actor, err := s.authorizeShare(ctx, action, owner)
	if err != nil {
		return "", err
	}
	if actor == "" {
		return "", status.Error(codes.PermissionDenied, "authenticated approver required")
	}
	if s.sharing == nil {
		return "", s.errNoStore()
	}
	return actor, nil
}

func (s *skillService) validateShare(a sharing.Artifact) error {
	var verify func([]byte, []byte) (string, bool)
	if s.verifier != nil {
		verify = s.verifier.Verify
	}
	if err := sharing.VerifyWith(a, s.policy == skills.SigningRequire, verify); err != nil {
		return err
	}
	p := a.Package()
	policy := s.policy
	if len(p.ManifestSignature) > 0 {
		policy = skills.SigningRequire
	}
	validator := &skillService{policy: policy, verifier: s.verifier}
	parsed, err := validator.parseBundle(&memory.Bundle{Manifest: p.Manifest, Signature: p.ManifestSignature, Files: p.Files})
	if err != nil {
		return err
	}
	if parsed.Name() != p.Name || parsed.Manifest.Version != p.Version {
		return errors.New("sharing: skill identity disagrees with manifest")
	}
	return nil
}

func (s *skillService) ExportShare(ctx context.Context, req *lobslawv1.ExportShareRequest) (*lobslawv1.ExportShareResponse, error) {
	actor, err := s.shareAccess(ctx, "skills:share:export", "")
	if err != nil {
		return nil, err
	}
	a, err := s.sharing.Export(req.GetName(), req.GetVersion(), req.GetScheduleIds(), actor, req.GetInputs(), req.GetSourceTimezone())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return &lobslawv1.ExportShareResponse{Artifact: a.Bytes()}, nil
}

func (s *skillService) InstallShare(ctx context.Context, req *lobslawv1.InstallShareRequest) (*lobslawv1.InstallShareResponse, error) {
	if _, err := s.shareAccess(ctx, "skills:share:install", req.GetOwner()); err != nil {
		return nil, err
	}
	a, err := sharing.Decode(req.GetArtifact())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.validateShare(a); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	p, err := s.sharing.PlanInstall(a, req.GetOwner(), req.GetInputs())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	raw, err := s.shareResult(ctx, p, a, req.GetApply(), req.GetExpectedPlan())
	if err != nil {
		return nil, err
	}
	return &lobslawv1.InstallShareResponse{PlanJson: raw}, nil
}

func (s *skillService) ActivateShare(ctx context.Context, req *lobslawv1.ActivateShareRequest) (*lobslawv1.ActivateShareResponse, error) {
	actor, err := s.shareAccess(ctx, "skills:share:activate", req.GetOwner())
	if err != nil {
		return nil, err
	}
	rec, err := s.sharing.Installation(req.GetInstallationId())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	if rec.Owner != req.GetOwner() {
		return nil, status.Error(codes.PermissionDenied, "installation owner does not match request")
	}
	a, err := sharing.Decode(rec.Artifact)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err := s.validateShare(a); err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	p, err := s.sharing.PlanActivate(rec.Id, rec.Owner, actor)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	raw, err := s.shareResult(ctx, p, a, req.GetApply(), req.GetExpectedPlan())
	if err != nil {
		return nil, err
	}
	return &lobslawv1.ActivateShareResponse{PlanJson: raw}, nil
}

func (s *skillService) shareResult(ctx context.Context, p *memory.SharePlan, a sharing.Artifact, apply bool, expected string) ([]byte, error) {
	if apply {
		if err := s.sharing.Apply(ctx, p, expected); err != nil {
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		}
	}
	// Include the original manifest and complete artifact fingerprint in the
	// review. Its declarations are requests, never policy grants.
	raw, err := json.Marshal(struct {
		*memory.SharePlan
		Manifest  string `json:"manifest"`
		Publisher string `json:"publisher,omitempty"`
		Applied   bool   `json:"applied"`
	}{p, string(a.Package().Manifest), a.Publisher(), apply})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return raw, nil
}
