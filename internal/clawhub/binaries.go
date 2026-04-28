package clawhub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jmylchreest/lobslaw/internal/egress"
)

// MaxBinarySize caps a single declared binary at 256 MiB. Skills
// shipping binaries are typically <50 MiB; the cap exists so a
// hostile catalog entry can't tar-bomb us indirectly via a 100 GB
// "binary" download.
const MaxBinarySize = 256 << 20

// binaryManifestSubset is the slice of skills.Manifest the binary
// pipeline reads. Replicated here so the clawhub package doesn't
// import internal/skills (which would create a cycle once skills
// gains a clawhub-aware install path). yaml tags match.
type binaryManifestSubset struct {
	Binaries []binaryDecl `yaml:"binaries"`
}

type binaryDecl struct {
	Name   string `yaml:"name"`
	URL    string `yaml:"url"`
	SHA256 string `yaml:"sha256"`
	Target string `yaml:"target"`
}

// fetchManifestBinaries reads manifest.yaml at staging/manifest.yaml
// and returns its binary declarations. Returns nil when the manifest
// declares no binaries; returns an error only when the manifest is
// unreadable / unparseable (the caller should already have validated
// the manifest exists).
func fetchManifestBinaries(stagingDir string) ([]binaryDecl, error) {
	raw, err := os.ReadFile(filepath.Join(stagingDir, "manifest.yaml"))
	if err != nil {
		return nil, fmt.Errorf("clawhub: read manifest: %w", err)
	}
	var m binaryManifestSubset
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("clawhub: parse manifest: %w", err)
	}
	return m.Binaries, nil
}

// installBinaries fetches every binary declared by the manifest
// inside stagingDir, places it at stagingDir/<target>, verifies
// SHA-256, and sets the executable bit. Runs after bundle extraction
// but before promoting the staging dir, so a failed binary download
// rolls back cleanly.
//
// HTTP routes through egress.For("clawhub") — same role as the
// catalog + bundle download, since binary hosts are configured via
// security.clawhub_binary_hosts under that role's allowlist.
func installBinaries(ctx context.Context, stagingDir string, decls []binaryDecl) error {
	if len(decls) == 0 {
		return nil
	}
	httpClient := egress.For("clawhub").HTTPClient()
	stagingClean := filepath.Clean(stagingDir) + string(os.PathSeparator)
	for _, d := range decls {
		if strings.TrimSpace(d.Name) == "" {
			return errors.New("clawhub: binary entry missing name")
		}
		if strings.TrimSpace(d.URL) == "" {
			return fmt.Errorf("clawhub: binary %q missing url", d.Name)
		}
		if strings.TrimSpace(d.SHA256) == "" {
			return fmt.Errorf("clawhub: binary %q missing sha256", d.Name)
		}
		if strings.TrimSpace(d.Target) == "" {
			return fmt.Errorf("clawhub: binary %q missing target", d.Name)
		}
		if err := guardEntryPath(d.Target); err != nil {
			return fmt.Errorf("clawhub: binary %q target: %w", d.Name, err)
		}
		dst := filepath.Join(stagingClean, filepath.Clean(d.Target))
		if !strings.HasPrefix(dst+string(os.PathSeparator), stagingClean) {
			return fmt.Errorf("clawhub: binary %q target %q escapes install root", d.Name, d.Target)
		}
		if err := downloadAndVerify(ctx, httpClient, d, dst); err != nil {
			return err
		}
	}
	return nil
}

func downloadAndVerify(ctx context.Context, client *http.Client, d binaryDecl, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("clawhub: mkdir for binary %q: %w", d.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("clawhub: GET binary %q: %w", d.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("clawhub: GET binary %q HTTP %d", d.Name, resp.StatusCode)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("clawhub: create %q: %w", dst, err)
	}
	hasher := sha256.New()
	limited := io.LimitReader(io.TeeReader(resp.Body, hasher), MaxBinarySize+1)
	written, err := io.Copy(out, limited)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("clawhub: write binary %q: %w", d.Name, err)
	}
	if written > MaxBinarySize {
		_ = os.Remove(dst)
		return fmt.Errorf("clawhub: binary %q exceeds %d byte cap", d.Name, MaxBinarySize)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if !strings.EqualFold(got, d.SHA256) {
		_ = os.Remove(dst)
		return fmt.Errorf("clawhub: binary %q SHA-256 mismatch: got %s, want %s", d.Name, got, d.SHA256)
	}
	return nil
}
