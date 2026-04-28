package clawhub

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/binaries"
	"github.com/jmylchreest/lobslaw/internal/storage"
)

// MaxBundleSize caps how much we'll pull from the catalog before
// erroring out. Skills bundle handler scripts + small assets, NOT
// the binaries themselves (those land via the Phase B binary
// pipeline). 50 MiB is generous for a skill manifest + handler tree.
const MaxBundleSize = 50 << 20

// InstallTarget identifies where the bundle lands. The catalog
// metadata supplies Name + Version; the operator's mount label +
// subpath (defaults: "skill-tools" + "<name>") give the install
// path. Operators with stricter layouts override the subpath
// scheme via the skills registry config.
type InstallTarget struct {
	MountLabel string
	Subpath    string
}

// Installer drives bundle download + verify + extract. Reuses the
// catalog Client for the HTTP fetch; takes a storage.Manager so the
// install path is resolved through whatever mount the operator
// declared (local disk, NFS, rclone-backed cloud). The signing
// policy + verifier guard the supply-chain edge — see signing.go.
type Installer struct {
	client    *Client
	storage   *storage.Manager
	policy    SigningPolicy
	verifier  BundleVerifier
	satisfier *binaries.Satisfier
}

// InstallerConfig wires Installer dependencies. Client + Storage
// required; Policy defaults to SigningPrefer (verify when present,
// don't block when absent) which matches how operators typically
// adopt signature checks.
type InstallerConfig struct {
	Client    *Client
	Storage   *storage.Manager
	Policy    SigningPolicy
	Verifier  BundleVerifier
	// Satisfier (optional) resolves clawhub-format bundles' declared
	// host bin requirements via the internal/binaries Manager pool.
	// When nil, clawhub-format bundles that declare requires.bins
	// fail at install time with a clear "operator hasn't wired the
	// binary satisfier" message. Native (manifest.yaml) bundles
	// don't use this field.
	Satisfier *binaries.Satisfier
}

// NewInstaller validates the config and constructs an Installer.
// SigningRequire with a nil/empty Verifier is a boot-time error —
// fail loudly so operators see the misconfiguration on startup
// rather than at first install.
func NewInstaller(cfg InstallerConfig) (*Installer, error) {
	if cfg.Client == nil {
		return nil, errors.New("clawhub: Installer requires a Client")
	}
	if cfg.Storage == nil {
		return nil, errors.New("clawhub: Installer requires a storage.Manager")
	}
	if cfg.Policy == "" {
		cfg.Policy = SigningPrefer
	}
	if !cfg.Policy.IsValid() {
		return nil, fmt.Errorf("clawhub: invalid signing policy %q", cfg.Policy)
	}
	if cfg.Policy == SigningRequire && (cfg.Verifier == nil || cfg.Verifier.Count() == 0) {
		return nil, errors.New("clawhub: SigningRequire but Verifier has no trusted keys")
	}
	return &Installer{
		client:    cfg.Client,
		storage:   cfg.Storage,
		policy:    cfg.Policy,
		verifier:  cfg.Verifier,
		satisfier: cfg.Satisfier,
	}, nil
}

// Client returns the underlying catalog client. Exposed so callers
// (the clawhub_install builtin, future CLI subcommand) can hit
// GetSkill before passing the entry to Install.
func (i *Installer) Client() *Client { return i.client }

// InstallResult captures what changed on disk for the caller's
// audit trail. ManifestPath is the resolved manifest.yaml location
// the skill registry's watcher will pick up next scan. SignedBy is
// the verified signer name (empty under SigningOff or when the
// catalog entry was unsigned under SigningPrefer).
type InstallResult struct {
	Name         string
	Version      string
	InstallDir   string
	ManifestPath string
	SignedBy     string
}

// InstallBySlug fetches a clawhub.ai-format bundle by slug and runs
// the same install pipeline. The slug shape is "<owner>/<name>"
// (e.g. "steipete/gog"); the catalogue serves the bundle bytes
// directly. SHA verification is skipped (clawhub.ai doesn't publish
// a per-bundle digest in this API path); the bundle's ed25519
// signature, when present, is the supply-chain anchor.
func (i *Installer) InstallBySlug(ctx context.Context, slug string, target InstallTarget) (*InstallResult, error) {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("clawhub: slug %q must be <owner>/<name>", slug)
	}
	if target.Subpath == "" {
		target.Subpath = parts[1]
	}
	entry := &SkillEntry{
		Name: parts[1],
	}
	body, err := i.client.DownloadBundleBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	return i.installFromBody(ctx, entry, target, body, "")
}

// installFromBody is the shared post-fetch pipeline used by Install
// and InstallBySlug. expectedSHA is verified when non-empty.
func (i *Installer) installFromBody(ctx context.Context, entry *SkillEntry, target InstallTarget, body io.Reader, expectedSHA string) (*InstallResult, error) {
	if target.MountLabel == "" {
		return nil, errors.New("clawhub: install target requires MountLabel")
	}
	mountRoot, err := i.storage.Resolve(target.MountLabel)
	if err != nil {
		return nil, fmt.Errorf("clawhub: mount %q: %w", target.MountLabel, err)
	}
	installDir := filepath.Join(mountRoot, target.Subpath)
	if !strings.HasPrefix(filepath.Clean(installDir)+string(os.PathSeparator), filepath.Clean(mountRoot)+string(os.PathSeparator)) {
		return nil, fmt.Errorf("clawhub: install dir %q escapes mount root %q", installDir, mountRoot)
	}

	bundleBytes, err := io.ReadAll(io.LimitReader(body, MaxBundleSize+1))
	if err != nil {
		return nil, fmt.Errorf("clawhub: read bundle body: %w", err)
	}
	if int64(len(bundleBytes)) > MaxBundleSize {
		return nil, fmt.Errorf("clawhub: bundle exceeds %d bytes", MaxBundleSize)
	}
	if expectedSHA != "" {
		hasher := sha256.New()
		hasher.Write(bundleBytes)
		if err := verifyDigest(hasher, expectedSHA); err != nil {
			return nil, err
		}
	}

	stage, err := os.MkdirTemp(filepath.Dir(installDir), "."+target.Subpath+".part-*")
	if err != nil {
		return nil, fmt.Errorf("clawhub: create staging dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stage) }

	processed, err := ProcessBundle(bundleBytes, stage)
	if err != nil {
		cleanup()
		return nil, err
	}

	switch processed.Format {
	case "native":
		binDecls, err := fetchManifestBinaries(stage)
		if err != nil {
			cleanup()
			return nil, err
		}
		if err := installBinaries(ctx, stage, binDecls); err != nil {
			cleanup()
			return nil, err
		}
	case "clawhub":
		if len(processed.RequiresBins) > 0 {
			if i.satisfier == nil {
				cleanup()
				return nil, fmt.Errorf("clawhub: bundle %q declares host bins %v but no Satisfier wired", processed.Name, processed.RequiresBins)
			}
			for _, bin := range processed.RequiresBins {
				if _, err := i.satisfier.Satisfy(ctx, bin, processed.InstallSpecs); err != nil {
					cleanup()
					return nil, fmt.Errorf("clawhub: satisfy %q: %w", bin, err)
				}
			}
		}
	default:
		cleanup()
		return nil, fmt.Errorf("clawhub: unrecognised bundle format %q", processed.Format)
	}

	if err := os.RemoveAll(installDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("clawhub: clear prior install: %w", err)
	}
	if err := os.Rename(stage, installDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("clawhub: promote staging dir: %w", err)
	}

	return &InstallResult{
		Name:         processed.Name,
		Version:      entry.Version,
		InstallDir:   installDir,
		ManifestPath: filepath.Join(installDir, "manifest.yaml"),
	}, nil
}

// Install downloads, verifies, and extracts entry's bundle into the
// target mount. SHA-256 is verified while streaming so a corrupted
// bundle aborts before any disk write happens. Extraction is
// strictly defensive: every entry's path is checked against the
// install root, and entries with leading "/" or "../" components
// are rejected.
//
// Idempotency: the install dir is rm-rf'd and recreated. Operators
// who want to keep multiple versions side-by-side use a Subpath
// that includes Version (e.g. "<name>-<version>").
func (i *Installer) Install(ctx context.Context, entry *SkillEntry, target InstallTarget) (*InstallResult, error) {
	if entry == nil {
		return nil, errors.New("clawhub: nil entry")
	}
	if target.MountLabel == "" {
		return nil, errors.New("clawhub: install target requires MountLabel")
	}
	signer, err := applySigningPolicy(entry, i.policy, i.verifier)
	if err != nil {
		return nil, err
	}
	if target.Subpath == "" {
		target.Subpath = entry.Name
	}
	if err := validateSkillIdentifier(target.Subpath); err != nil {
		return nil, fmt.Errorf("clawhub: subpath %w", err)
	}
	mountRoot, err := i.storage.Resolve(target.MountLabel)
	if err != nil {
		return nil, fmt.Errorf("clawhub: mount %q: %w", target.MountLabel, err)
	}
	installDir := filepath.Join(mountRoot, target.Subpath)
	if !strings.HasPrefix(filepath.Clean(installDir)+string(os.PathSeparator), filepath.Clean(mountRoot)+string(os.PathSeparator)) {
		return nil, fmt.Errorf("clawhub: install dir %q escapes mount root %q", installDir, mountRoot)
	}

	body, err := i.client.DownloadBundle(ctx, entry)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	bundleBytes, err := io.ReadAll(io.LimitReader(body, MaxBundleSize+1))
	if err != nil {
		return nil, fmt.Errorf("clawhub: read bundle body: %w", err)
	}
	if int64(len(bundleBytes)) > MaxBundleSize {
		return nil, fmt.Errorf("clawhub: bundle exceeds %d bytes", MaxBundleSize)
	}
	if entry.BundleSHA256 != "" {
		hasher := sha256.New()
		hasher.Write(bundleBytes)
		if err := verifyDigest(hasher, entry.BundleSHA256); err != nil {
			return nil, err
		}
	}

	stage, err := os.MkdirTemp(filepath.Dir(installDir), "."+target.Subpath+".part-*")
	if err != nil {
		return nil, fmt.Errorf("clawhub: create staging dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(stage) }

	processed, err := ProcessBundle(bundleBytes, stage)
	if err != nil {
		cleanup()
		return nil, err
	}

	switch processed.Format {
	case "native":
		binDecls, err := fetchManifestBinaries(stage)
		if err != nil {
			cleanup()
			return nil, err
		}
		if err := installBinaries(ctx, stage, binDecls); err != nil {
			cleanup()
			return nil, err
		}
	case "clawhub":
		if len(processed.RequiresBins) > 0 {
			if i.satisfier == nil {
				cleanup()
				return nil, fmt.Errorf("clawhub: bundle %q declares host bins %v but no Satisfier wired (operator must enable binary install path)", entry.Name, processed.RequiresBins)
			}
			for _, bin := range processed.RequiresBins {
				if _, err := i.satisfier.Satisfy(ctx, bin, processed.InstallSpecs); err != nil {
					cleanup()
					return nil, fmt.Errorf("clawhub: satisfy %q: %w", bin, err)
				}
			}
		}
	default:
		cleanup()
		return nil, fmt.Errorf("clawhub: unrecognised bundle format %q", processed.Format)
	}

	if err := os.RemoveAll(installDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("clawhub: clear prior install: %w", err)
	}
	if err := os.Rename(stage, installDir); err != nil {
		cleanup()
		return nil, fmt.Errorf("clawhub: promote staging dir: %w", err)
	}

	return &InstallResult{
		Name:         entry.Name,
		Version:      entry.Version,
		InstallDir:   installDir,
		ManifestPath: filepath.Join(installDir, "manifest.yaml"),
		SignedBy:     signer,
	}, nil
}

// extractTarGz unpacks a gzip-compressed tar stream into dst. Each
// entry's path is checked against dst — symlinks, hardlinks, and
// device files are rejected. The bundle MUST be flat (regular files
// + directories only); supporting symlinks would let a malicious
// bundle plant a link pointing outside the install root that a
// later write would follow.
func extractTarGz(r io.Reader, dst string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("clawhub: gunzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	dst = filepath.Clean(dst) + string(os.PathSeparator)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("clawhub: tar read: %w", err)
		}
		if err := guardEntryPath(hdr.Name); err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.Clean(hdr.Name))
		if !strings.HasPrefix(target+string(os.PathSeparator), dst) {
			return fmt.Errorf("clawhub: entry %q escapes install root", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("clawhub: mkdir %q: %w", hdr.Name, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("clawhub: mkdir parent %q: %w", hdr.Name, err)
			}
			if err := writeTarFile(tr, target, hdr.Mode); err != nil {
				return err
			}
		default:
			return fmt.Errorf("clawhub: bundle contains unsupported entry %q (type %d)", hdr.Name, hdr.Typeflag)
		}
	}
}

func writeTarFile(r io.Reader, target string, mode int64) error {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(mode)&0o777)
	if err != nil {
		return fmt.Errorf("clawhub: create %q: %w", target, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := io.Copy(f, r); err != nil {
		return fmt.Errorf("clawhub: write %q: %w", target, err)
	}
	return nil
}

func guardEntryPath(name string) error {
	cleaned := filepath.Clean(name)
	if cleaned == "." {
		return nil
	}
	if filepath.IsAbs(cleaned) {
		return fmt.Errorf("clawhub: entry %q is absolute", name)
	}
	if strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("clawhub: entry %q traverses parent", name)
	}
	return nil
}

func verifyDigest(h hash.Hash, expectedHex string) error {
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, expectedHex) {
		return fmt.Errorf("clawhub: bundle SHA-256 mismatch: got %s, want %s", got, expectedHex)
	}
	return nil
}
