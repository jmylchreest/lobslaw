package node

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/grpcinterceptors"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/skills"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func (n *Node) wireArchiveService() error {
	lobslawv1.RegisterArchiveServiceServer(n.server, memory.NewArchiveRPC(
		n.memorySvc, n.authorizeArchive, n.validateArchiveSkills))
	return nil
}

func (n *Node) authorizeArchive(ctx context.Context, action string) error {
	cert := grpcinterceptors.VerifiedPeerCert(ctx)
	if cert == nil || !mtls.IsOperatorCert(cert) || n.policyEngine == nil {
		return status.Error(codes.PermissionDenied, "archive requires an operator certificate and a data policy grant")
	}
	roles := n.resolveUserRoles(cert.Subject.CommonName)
	if !slices.Contains(roles, "operator") {
		return status.Error(codes.PermissionDenied, "certificate identity must hold the configured operator data role")
	}
	claims := &types.Claims{UserID: cert.Subject.CommonName, Roles: roles}
	decision, err := n.policyEngine.Evaluate(ctx, claims, action, "memory:*")
	if err != nil || decision.Effect != types.EffectAllow {
		return status.Error(codes.PermissionDenied, "archive action requires an explicit data policy grant")
	}
	return nil
}

// Imported archives cannot assert a signing tier. Verify signed records even
// on a deployment whose ordinary signing policy is off, before storing them.
func (n *Node) validateArchiveSkills(_ context.Context, records []archive.Record) error {
	blobs := make(map[string][]byte)
	for _, record := range records {
		if record.Kind != "skill-blobs" {
			continue
		}
		var blob lobslawv1.SkillBlob
		if err := protojson.Unmarshal(record.Data, &blob); err != nil {
			return errors.New("invalid skill blob")
		}
		blobs[blob.Digest] = blob.Content
	}
	for _, record := range records {
		if record.Kind != "skills" {
			continue
		}
		var skill lobslawv1.SkillRecord
		if err := protojson.Unmarshal(record.Data, &skill); err != nil {
			return errors.New("invalid skill record")
		}
		bundle := &memory.Bundle{
			Manifest: skill.ManifestYaml, Signature: skill.ManifestSig,
			Files: make(map[string][]byte),
		}
		for path, digest := range skill.Files {
			content, ok := blobs[digest]
			if !ok {
				return errors.New("skill requires missing blob")
			}
			bundle.Files[path] = content
		}
		policy := n.skillSigningPolicy
		if skill.Tier == lobslawv1.SkillTier_SKILL_TIER_SIGNED {
			policy = skills.SigningRequire
		}
		validator := &skillService{policy: policy, verifier: n.skillVerifier}
		parsed, err := validator.parseBundle(bundle)
		if err != nil {
			return err
		}
		if parsed.Name() != skill.Name || parsed.Manifest.Version != skill.Version {
			return errors.New("skill identity disagrees with its manifest")
		}
	}
	return nil
}
