package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalizeAndContainRejectsRelative(t *testing.T) {
	t.Parallel()
	_, err := CanonicalizeAndContain("relative/path", nil)
	if err == nil {
		t.Error("relative path should be rejected")
	}
}

func TestCanonicalizeAndContainRejectsEmpty(t *testing.T) {
	t.Parallel()
	_, err := CanonicalizeAndContain("", nil)
	if err == nil {
		t.Error("empty path should be rejected")
	}
}

func TestCanonicalizeAndContainRespectsRoots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	inside := filepath.Join(dir, "inside.txt")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalizeAndContain(inside, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	wantDir, _ := filepath.EvalSymlinks(dir)
	if !strings.HasPrefix(got, wantDir) {
		t.Errorf("got %q, want it to be under %q", got, wantDir)
	}
}

func TestCanonicalizeAndContainRejectsOutsideRoots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := "/etc/passwd"
	if _, err := os.Stat(outside); os.IsNotExist(err) {
		t.Skipf("system lacks %s", outside)
	}
	_, err := CanonicalizeAndContain(outside, []string{dir})
	if err == nil {
		t.Error("SECURITY: path outside roots accepted")
	}
}

func TestCanonicalizeAndContainFollowsSymlinkInsideRoots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "alias")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalizeAndContain(link, []string{dir})
	if err != nil {
		t.Fatal(err)
	}
	realResolved, _ := filepath.EvalSymlinks(real)
	if got != realResolved {
		t.Errorf("got %q, want %q (symlink should resolve)", got, realResolved)
	}
}

func TestCanonicalizeAndContainRejectsSymlinkToOutside(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := "/etc/passwd"
	if _, err := os.Stat(outside); os.IsNotExist(err) {
		t.Skipf("system lacks %s", outside)
	}
	trap := filepath.Join(dir, "trap")
	if err := os.Symlink(outside, trap); err != nil {
		t.Fatal(err)
	}
	_, err := CanonicalizeAndContain(trap, []string{dir})
	if err == nil {
		t.Error("SECURITY: symlink to outside target was accepted")
	}
}

func TestRequireSingleLinkAcceptsUnlinkedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "alone")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RequireSingleLink(file); err != nil {
		t.Errorf("single-linked file rejected: %v", err)
	}
}

func TestRequireSingleLinkRejectsHardlink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(a, b); err != nil {
		t.Skipf("hardlink not supported on %s: %v", dir, err)
	}
	if err := RequireSingleLink(a); err == nil {
		t.Error("SECURITY: hardlinked file was not rejected by RequireSingleLink")
	}
}

func TestRequireSingleLinkRejectsDirectory(t *testing.T) {
	t.Parallel()
	if err := RequireSingleLink(t.TempDir()); err == nil {
		t.Error("directory should be rejected (nlink check is file-only)")
	}
}
