package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateReplacementAndFailedRename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := WritePrivate(path, []byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := WritePrivate(path, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private mode lost: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "replacement" {
		t.Fatalf("bad replacement: %q %v", raw, err)
	}
	if err := WritePrivate(filepath.Dir(path), []byte("invalid")); err == nil {
		t.Fatal("rename over directory unexpectedly succeeded")
	}
	raw, err = os.ReadFile(path)
	if err != nil || string(raw) != "replacement" {
		t.Fatal("failed rename damaged saved state")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary state leaked: %v %v", entries, err)
	}
}
