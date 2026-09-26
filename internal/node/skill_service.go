package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/skills"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Installing skills on a running cluster.
//
// The bytes travel, not a path. `lobslaw skills import` runs on
// somebody's laptop and the cluster is elsewhere, so a service taking a
// directory would be reading one that does not exist on the node —
// and the failure would be a confusing "no such file" naming a path
// that exists perfectly well on the machine that sent it.

// skillService serves the skill store over gRPC.
type skillService struct {
	lobslawv1.UnimplementedSkillServiceServer
	store          *memory.SkillStore
	sharing        *memory.SharingStore
	authorizeShare func(context.Context, string, string) (string, error)
	// policy and verifier are the node's signing stance, applied to an
	// import the same way the mount importer applies it. An unsigned
	// skill pushed into a SigningRequire cluster is refused HERE
	// rather than replicated and then failing to load everywhere.
	policy   skills.SigningPolicy
	verifier *skills.Verifier
}

func (s *skillService) errNoStore() error {
	return status.Error(codes.FailedPrecondition,
		"this node has no skill store; skills require the memory function and raft")
}

func (s *skillService) ImportSkill(ctx context.Context, req *lobslawv1.ImportSkillRequest) (*lobslawv1.ImportSkillResponse, error) {
	name := strings.TrimSpace(req.GetName())
	version := strings.TrimSpace(req.GetVersion())
	switch {
	case name == "":
		return nil, status.Error(codes.InvalidArgument, "name is required")
	case version == "":
		return nil, status.Error(codes.InvalidArgument, "version is required")
	case len(req.GetManifestYaml()) == 0:
		return nil, status.Error(codes.InvalidArgument, "manifest_yaml is required")
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	bundle := &memory.Bundle{
		Manifest:  req.GetManifestYaml(),
		Signature: req.GetManifestSig(),
		Files:     req.GetFiles(),
	}
	if err := s.validate(bundle); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	rec, err := s.store.Import(ctx, memory.ImportRequest{
		Name: name, Version: version, Tier: req.GetTier(),
		Bundle:     bundle,
		Source:     strings.TrimSpace(req.GetSource()),
		ImportedBy: strings.TrimSpace(req.GetImportedBy()),
		Activate:   req.GetActivate(),
	})
	if err != nil {
		return nil, skillError(err)
	}
	return &lobslawv1.ImportSkillResponse{Skill: rec}, nil
}

func (s *skillService) ExportSkill(_ context.Context, req *lobslawv1.ExportSkillRequest) (*lobslawv1.ExportSkillResponse, error) {
	name, version := strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetVersion())
	if name == "" || version == "" {
		return nil, status.Error(codes.InvalidArgument, "name and version are required")
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	rec, err := s.store.Get(name, version)
	if err != nil {
		return nil, skillError(err)
	}
	files := make(map[string][]byte, len(rec.GetFiles()))
	for rel, digest := range rec.GetFiles() {
		content, err := s.store.Blob(digest)
		if err != nil {
			// The whole export fails rather than returning a partial
			// bundle. A skill missing its handler is not a degraded
			// skill; it is one that fails at invoke, and writing it to
			// disk would produce a directory that looks complete.
			return nil, skillError(err)
		}
		files[rel] = content
	}
	return &lobslawv1.ExportSkillResponse{
		ManifestYaml: rec.GetManifestYaml(),
		ManifestSig:  rec.GetManifestSig(),
		Files:        files,
	}, nil
}

func (s *skillService) ListSkills(_ context.Context, req *lobslawv1.ListSkillsRequest) (*lobslawv1.ListSkillsResponse, error) {
	if s.store == nil {
		return nil, s.errNoStore()
	}
	var (
		records []*lobslawv1.SkillRecord
		err     error
	)
	if req.GetActiveOnly() {
		records, err = s.store.Active()
	} else {
		records, err = s.store.List()
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &lobslawv1.ListSkillsResponse{Skills: records}, nil
}

func (s *skillService) RemoveSkill(ctx context.Context, req *lobslawv1.RemoveSkillRequest) (*lobslawv1.RemoveSkillResponse, error) {
	name, version := strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetVersion())
	if name == "" || version == "" {
		return nil, status.Error(codes.InvalidArgument, "name and version are required")
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	if err := s.store.Remove(ctx, name, version); err != nil {
		return nil, skillError(err)
	}
	return &lobslawv1.RemoveSkillResponse{}, nil
}

// validate parses the bundle through the REAL loader before storing
// it.
//
// Written to a temporary directory and parsed, rather than
// signature-checked in isolation, so an import is held to exactly the
// standard a load is. Verifying the signature by hand here would admit
// a signed manifest that pins no handler digest — which ParseWithPolicy
// refuses, because a signature naming a script but not its digest
// covers no executable content — and the skill would replicate to
// every node and fail to load on all of them.
//
// The feedback belongs at the door, where somebody is watching.
func (s *skillService) validate(bundle *memory.Bundle) error {
	_, err := s.parseBundle(bundle)
	return err
}

func (s *skillService) parseBundle(bundle *memory.Bundle) (*skills.Skill, error) {
	dir, err := os.MkdirTemp("", "lobslaw-import-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()

	if err := os.WriteFile(filepath.Join(dir, memory.ManifestFile), bundle.Manifest, 0o600); err != nil {
		return nil, err
	}
	if len(bundle.Signature) > 0 {
		if err := os.WriteFile(filepath.Join(dir, memory.SignatureFile), bundle.Signature, 0o600); err != nil {
			return nil, err
		}
	}
	for rel, content := range bundle.Files {
		// The same traversal check the materialiser applies. A bundle
		// arriving over the wire is less trustworthy than one read from
		// a local directory, not more.
		cleaned := filepath.Clean(filepath.FromSlash(rel))
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") {
			return nil, fmt.Errorf("bundled file %q is outside the skill directory", rel)
		}
		dest := filepath.Join(dir, cleaned)
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dest, content, 0o600); err != nil {
			return nil, err
		}
	}
	return skills.ParseWithPolicy(dir, s.policy, s.verifier)
}

// ActivateSkill makes a stored version the one in force — the whole of
// a rollback, since every version imported is still in the log.
func (s *skillService) ActivateSkill(ctx context.Context, req *lobslawv1.ActivateSkillRequest) (*lobslawv1.ActivateSkillResponse, error) {
	name, version := strings.TrimSpace(req.GetName()), strings.TrimSpace(req.GetVersion())
	if name == "" || version == "" {
		return nil, status.Error(codes.InvalidArgument, "name and version are required")
	}
	if s.store == nil {
		return nil, s.errNoStore()
	}
	// NOT re-validated through the loader. The record was parsed when
	// it arrived; re-parsing it here would refuse a skill that a
	// tightened signing policy no longer admits, which is exactly the
	// situation somebody rolling back is trying to escape.
	rec, already, err := s.store.Activate(ctx, name, version)
	if err != nil {
		return nil, skillError(err)
	}
	return &lobslawv1.ActivateSkillResponse{Skill: rec, AlreadyActive: already}, nil
}

// skillError maps store errors onto gRPC codes.
//
// "Not found" and "too large" send an operator to different places —
// one is a typo in a name, the other is a bundle that needs a file
// moving to storage — and a CLI can only say which if the code carries
// it.
func skillError(err error) error {
	switch {
	case errors.Is(err, memory.ErrSkillNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, memory.ErrSkillTooLarge):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, memory.ErrNoManifest):
		return status.Error(codes.InvalidArgument, err.Error())
	default:
		return status.Error(codes.Internal, err.Error())
	}
}
