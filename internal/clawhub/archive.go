package clawhub

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MaxBundleSize bounds catalogue downloads before the stricter portable-package checks.
const MaxBundleSize = 50 << 20

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

// readAndVerifyBundle reads a bundle under the size cap and checks it
// against a declared digest.
//
// One byte over the cap, so a bundle exactly AT the limit reads whole
// and only a genuinely oversized one trips it.
//
// An empty expectedSHA skips the check rather than failing: the two
// callers differ on whether a digest is available, and refusing here
// would move that decision away from the caller that knows.
func readAndVerifyBundle(body io.Reader, expectedSHA string) ([]byte, error) {
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
	return bundleBytes, nil
}
