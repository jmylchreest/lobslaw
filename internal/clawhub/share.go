package clawhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jmylchreest/lobslaw/internal/sharing"
	"github.com/jmylchreest/lobslaw/internal/skills"
)

// ShareSource prepares an artifact without promoting a mount, bootstrapping
// binaries, or writing policy. Installation remains the destination's job.
type ShareSource struct {
	client   *Client
	verifier BundleVerifier
}

func NewShareSource(base string, verifier BundleVerifier) (*ShareSource, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("clawhub: catalogue must be an HTTP(S) URL without credentials, query or fragment")
	}
	client, err := NewClient(base)
	if err != nil {
		return nil, err
	}
	return &ShareSource{client: client, verifier: verifier}, nil
}

func (s *ShareSource) Fetch(ctx context.Context, ref string) (sharing.Artifact, error) {
	name, version, err := shareReference(ref)
	if err != nil {
		return sharing.Artifact{}, err
	}
	raw, err := s.download(ctx, name, version)
	if err != nil {
		return sharing.Artifact{}, err
	}
	if err := checkShareArchive(raw); err != nil {
		return sharing.Artifact{}, err
	}
	dir, err := os.MkdirTemp("", "lobslaw-clawhub-share-")
	if err != nil {
		return sharing.Artifact{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	processed, err := ProcessBundle(raw, dir)
	if err != nil {
		return sharing.Artifact{}, err
	}
	if processed.Name != name {
		return sharing.Artifact{}, errors.New("clawhub: bundle identity differs from requested skill")
	}
	if processed.Format == "clawhub" {
		if err := prepareSharedManifest(dir, name, version); err != nil {
			return sharing.Artifact{}, err
		}
	}
	// Conversion validates structure and declared file digests. Preserve the
	// manifest signature below: the destination verifies it against its own
	// trusted publishers before staging, activation and scheduled execution.
	parsed, err := skills.ParseWithPolicy(dir, skills.SigningOff, nil)
	if err != nil {
		return sharing.Artifact{}, err
	}
	if version != "" && parsed.Manifest.Version != version {
		return sharing.Artifact{}, errors.New("clawhub: bundle version differs from requested version")
	}
	if err := ctx.Err(); err != nil {
		return sharing.Artifact{}, err
	}
	p := sharing.Package{Format: sharing.Format, Schema: 1, Name: parsed.Name(), Version: parsed.Manifest.Version, Files: make(map[string][]byte), Origin: &sharing.Origin{Reference: ref, Catalog: s.client.baseURL, Digest: sharing.Hash(raw), Format: processed.Format}}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch rel {
		case "manifest.yaml":
			p.Manifest = content
		case "manifest.yaml.sig":
			p.ManifestSignature = content
		default:
			p.Files[filepath.ToSlash(rel)] = content
		}
		return nil
	})
	if err != nil {
		return sharing.Artifact{}, err
	}
	return sharing.Build(p)
}

func shareReference(ref string) (string, string, error) {
	tail, ok := strings.CutPrefix(ref, "clawhub:")
	if !ok {
		return "", "", errors.New("clawhub: expected clawhub:<slug> or clawhub:<name>@<version>")
	}
	name, version := tail, ""
	if at := strings.LastIndexByte(tail, '@'); at >= 0 {
		name, version = tail[:at], tail[at+1:]
		if version == "" {
			return "", "", errors.New("clawhub: empty version")
		}
		if err := validateSkillIdentifier(version); err != nil {
			return "", "", err
		}
	}
	name, err := normalizeSlug(name)
	return name, version, err
}

func (s *ShareSource) download(ctx context.Context, name, version string) ([]byte, error) {
	if version == "" {
		body, err := s.client.DownloadBundleBySlug(ctx, name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = body.Close() }()
		return readAndVerifyBundle(body, "")
	}
	entry, err := s.client.GetSkill(ctx, name, version)
	if err != nil {
		return nil, err
	}
	if entry.Name != name || entry.Version != version {
		return nil, errors.New("clawhub: catalogue identity differs from request")
	}
	// Never silently ignore a present catalogue signature, even without keys.
	if entry.Signature != "" || entry.SignedBy != "" {
		if _, err := applySigningPolicy(entry, SigningRequire, s.verifier); err != nil {
			return nil, err
		}
	}
	body, err := s.client.DownloadBundle(ctx, entry)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return readAndVerifyBundle(body, entry.BundleSHA256)
}

// Only the synthetic manifest is rewritten. The upstream SKILL.md and assets
// remain exact bytes; upstream signatures never become sharing signatures.
func prepareSharedManifest(dir, name, version string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "_meta.json"))
	if err == nil {
		var meta struct{ Slug, Version string }
		if err := json.Unmarshal(raw, &meta); err != nil {
			return fmt.Errorf("clawhub: invalid _meta.json: %w", err)
		}
		if meta.Slug != "" && meta.Slug != name {
			return errors.New("clawhub: metadata slug differs from request")
		}
		if version != "" && meta.Version != "" && meta.Version != version {
			return errors.New("clawhub: metadata version differs from request")
		}
		if meta.Version != "" {
			version = meta.Version
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	path := filepath.Join(dir, "manifest.yaml")
	raw, err = os.ReadFile(path)
	if err != nil {
		return err
	}
	var manifest map[string]any
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	if version != "" {
		manifest["version"] = version
	}
	manifest["body"] = "SKILL.md"
	for field, file := range map[string]string{"body_sha256": "SKILL.md", "handler_sha256": "handler.sh"} {
		content, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return err
		}
		manifest[field] = strings.TrimPrefix(sharing.Hash(content), "sha256:")
	}
	raw, err = yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
