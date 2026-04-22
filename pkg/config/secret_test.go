package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestResolveSecretEmpty(t *testing.T) {
	t.Parallel()
	got, err := ResolveSecret("")
	if err != nil {
		t.Fatalf("empty ref should not error: %v", err)
	}
	if got != "" {
		t.Errorf("empty ref should return empty, got %q", got)
	}
}

func TestResolveSecretEnv(t *testing.T) {
	t.Setenv("LOBSLAW_TEST_SECRET", "sekrit")

	got, err := ResolveSecret("env:LOBSLAW_TEST_SECRET")
	if err != nil {
		t.Fatalf("ResolveSecret: %v", err)
	}
	if got != "sekrit" {
		t.Errorf("got %q, want sekrit", got)
	}
}

func TestResolveSecretEnvMissing(t *testing.T) {
	t.Parallel()
	_, err := ResolveSecret("env:LOBSLAW_TEST_DEFINITELY_NOT_SET_XYZ")
	if !errors.Is(err, types.ErrMissingSecret) {
		t.Errorf("err = %v, want wraps ErrMissingSecret", err)
	}
}

func TestResolveSecretFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := ResolveSecret("file:" + path)
	if err != nil {
		t.Fatalf("ResolveSecret: %v", err)
	}
	if got != "file-secret" {
		t.Errorf("got %q, want file-secret (trailing newline trimmed)", got)
	}
}

func TestResolveSecretFileMissing(t *testing.T) {
	t.Parallel()
	_, err := ResolveSecret("file:/nonexistent/path/xyz")
	if !errors.Is(err, types.ErrMissingSecret) {
		t.Errorf("err = %v, want wraps ErrMissingSecret", err)
	}
}

func TestResolveSecretUnknownScheme(t *testing.T) {
	t.Parallel()
	_, err := ResolveSecret("vault:secret/data/foo")
	if !errors.Is(err, types.ErrUnknownSecretScheme) {
		t.Errorf("err = %v, want wraps ErrUnknownSecretScheme", err)
	}
}

func TestResolveSecretNoScheme(t *testing.T) {
	t.Parallel()
	_, err := ResolveSecret("literal-secret-value")
	if !errors.Is(err, types.ErrUnknownSecretScheme) {
		t.Errorf("err = %v, want reject literal values", err)
	}
}
