package binaries

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ghReleaseManager downloads a binary asset from a GitHub release
// URL and lands it under $prefix/bin/<name>. Supports tar.gz, tar,
// zip, or single-binary archives — auto-detected by magic bytes.
//
// InstallSpec shape this manager expects:
//
//	Manager:  "gh-release"
//	Package:  the binary's name on PATH after install (e.g. "gog")
//	URL:      full asset URL, e.g.
//	          "https://github.com/owner/repo/releases/download/v1.0/asset_linux_amd64.tar.gz"
//	Checksum: optional "sha256:<hex>" — verified before extract
//	Args:     optional ["asset-relative-path-inside-archive"]; defaults
//	          to the binary name. Lets operators point at e.g. "bin/gog"
//	          when the archive has a nested layout.
type ghReleaseManager struct {
	prefix     string
	httpClient *http.Client
}

func (ghReleaseManager) Name() string   { return "gh-release" }
func (ghReleaseManager) UserMode() bool { return true }

func (ghReleaseManager) Hosts(spec InstallSpec) []string {
	hosts := []string{
		"github.com",
		"objects.githubusercontent.com",
		"release-assets.githubusercontent.com",
	}
	if spec.URL != "" {
		if u, err := url.Parse(spec.URL); err == nil && u.Hostname() != "" {
			hosts = append(hosts, u.Hostname())
		}
	}
	return hosts
}

// Available returns true whenever the manager is wired (i.e. an
// HTTPClient was supplied to the Satisfier). All extraction happens
// in-process, no shell-out.
func (m ghReleaseManager) Available(_ context.Context) bool {
	return m.httpClient != nil
}

func (m ghReleaseManager) Install(ctx context.Context, spec InstallSpec, _ ProcessRunner, log *slog.Logger) error {
	if spec.URL == "" {
		return errors.New("gh-release: spec.URL required")
	}
	if spec.Package == "" {
		return errors.New("gh-release: spec.Package (binary name on PATH) required")
	}
	if m.prefix == "" {
		return errors.New("gh-release: Satisfier prefix required")
	}
	binDir := m.prefix + "/bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("gh-release: mkdir %s: %w", binDir, err)
	}

	log.Info("gh-release: fetch", "url", spec.URL, "name", spec.Package)
	body, err := fetchAsset(ctx, m.httpClient, spec.URL)
	if err != nil {
		return fmt.Errorf("gh-release: fetch: %w", err)
	}

	if spec.Checksum != "" {
		expected := strings.TrimPrefix(spec.Checksum, "sha256:")
		got := sha256.Sum256(body)
		if hex.EncodeToString(got[:]) != expected {
			return fmt.Errorf("gh-release: checksum mismatch (got sha256:%s, want %s)", hex.EncodeToString(got[:]), spec.Checksum)
		}
	}

	innerPath := spec.Package
	if len(spec.Args) > 0 && spec.Args[0] != "" {
		innerPath = spec.Args[0]
	}

	target := binDir + "/" + spec.Package
	tmp := target + ".part"
	defer os.Remove(tmp)

	switch detectArchive(body) {
	case "tar.gz":
		if err := extractFromTarGz(body, innerPath, tmp); err != nil {
			return fmt.Errorf("gh-release: extract %s: %w", innerPath, err)
		}
	case "tar":
		if err := extractFromTar(bytes.NewReader(body), innerPath, tmp); err != nil {
			return fmt.Errorf("gh-release: extract %s: %w", innerPath, err)
		}
	case "zip":
		if err := extractFromZip(body, innerPath, tmp); err != nil {
			return fmt.Errorf("gh-release: extract %s: %w", innerPath, err)
		}
	default:
		// Bare binary — no archive, write directly.
		if err := os.WriteFile(tmp, body, 0o755); err != nil {
			return fmt.Errorf("gh-release: write tempfile: %w", err)
		}
	}

	if err := os.Chmod(tmp, 0o755); err != nil {
		return fmt.Errorf("gh-release: chmod: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("gh-release: rename %s → %s: %w", tmp, target, err)
	}
	log.Info("gh-release: installed", "binary", target, "name", spec.Package)
	return nil
}

func detectArchive(b []byte) string {
	if len(b) >= 2 && b[0] == 0x1f && b[1] == 0x8b {
		return "tar.gz"
	}
	if len(b) >= 4 && b[0] == 0x50 && b[1] == 0x4b && (b[2] == 0x03 || b[2] == 0x05 || b[2] == 0x07) {
		return "zip"
	}
	if len(b) >= 263 && string(b[257:262]) == "ustar" {
		return "tar"
	}
	return ""
}

func fetchAsset(ctx context.Context, client *http.Client, urlStr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	const maxAsset = 256 << 20 // 256 MiB cap; plenty for any CLI release
	return io.ReadAll(io.LimitReader(resp.Body, maxAsset+1))
}

func extractFromTarGz(body []byte, innerPath, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer gz.Close()
	return extractFromTar(gz, innerPath, dst)
}

func extractFromTar(r io.Reader, innerPath, dst string) error {
	tr := tar.NewReader(r)
	candidates := buildPathCandidates(innerPath)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("not found in archive: %v", candidates)
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		clean := filepath.Clean(hdr.Name)
		for _, c := range candidates {
			if clean == c || strings.HasSuffix(clean, "/"+c) {
				return writeStreamToFile(tr, dst)
			}
		}
	}
}

func extractFromZip(body []byte, innerPath, dst string) error {
	rdr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}
	candidates := buildPathCandidates(innerPath)
	for _, f := range rdr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		clean := filepath.Clean(f.Name)
		for _, c := range candidates {
			if clean == c || strings.HasSuffix(clean, "/"+c) {
				rc, err := f.Open()
				if err != nil {
					return err
				}
				defer rc.Close()
				return writeStreamToFile(rc, dst)
			}
		}
	}
	return fmt.Errorf("not found in archive: %v", candidates)
}

func writeStreamToFile(r io.Reader, dst string) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, r); err != nil {
		return err
	}
	return out.Close()
}

// buildPathCandidates returns the set of paths inside the archive
// to look for, ordered most-specific first. Operators can pass
// "bin/gog" as Args[0] for a nested layout; we also try the bare
// name and a few common placements.
func buildPathCandidates(inner string) []string {
	clean := filepath.Clean(inner)
	out := []string{clean}
	bare := filepath.Base(clean)
	if bare != clean {
		out = append(out, bare)
	}
	out = append(out, "bin/"+bare)
	return out
}
