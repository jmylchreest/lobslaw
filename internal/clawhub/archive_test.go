package clawhub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func makeBundle(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestExtractTarGzRejectsTraversalEntry(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escape.txt", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	dst := t.TempDir()
	err := extractTarGz(&buf, dst)
	if err == nil || !strings.Contains(err.Error(), "traverses parent") {
		t.Errorf("expected traversal rejection, got %v", err)
	}
}

func TestExtractTarGzRejectsAbsoluteEntry(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name: "/etc/passwd", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	err := extractTarGz(&buf, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("expected absolute-path rejection, got %v", err)
	}
}

func TestExtractTarGzRejectsSymlink(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{
		Name: "evil", Linkname: "../target", Typeflag: tar.TypeSymlink,
	})
	_ = tw.Close()
	_ = gz.Close()
	err := extractTarGz(&buf, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unsupported entry") {
		t.Errorf("expected symlink rejection, got %v", err)
	}
}
