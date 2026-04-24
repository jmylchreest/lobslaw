package compute

import (
	"errors"
	"strings"
	"testing"
)

func TestMountResolverPassesAbsolutePathThrough(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	got, err := r.Resolve("/abs/path", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/abs/path" {
		t.Errorf("absolute pass-through: %q", got)
	}
}

func TestMountResolverRejectsUnknownLabel(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	_, err := r.Resolve("workspace/foo.md", false)
	if !errors.Is(err, ErrMountNotFound) {
		t.Errorf("err=%v; want ErrMountNotFound", err)
	}
}

func TestMountResolverResolvesSubpath(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", true, nil)
	got, err := r.Resolve("workspace/notes.md", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/u/ws/notes.md" {
		t.Errorf("resolved: %q", got)
	}
}

func TestMountResolverRejectsTraversal(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", true, nil)
	_, err := r.Resolve("workspace/../../../etc/passwd", false)
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err=%v; want ErrPathEscape", err)
	}
}

func TestMountResolverEnforcesReadOnly(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("snaps", "/data/snap", false, nil)
	_, err := r.Resolve("snaps/a.db", true)
	if !errors.Is(err, ErrReadOnlyMount) {
		t.Errorf("err=%v; want ErrReadOnlyMount", err)
	}
	// Read on a read-only mount is fine.
	got, err := r.Resolve("snaps/a.db", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "/data/snap/") {
		t.Errorf("read should succeed: %q", got)
	}
}

func TestMountResolverAppliesExcludes(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", true, []string{"secret.*"})
	_, err := r.Resolve("workspace/secret.key", false)
	if !errors.Is(err, ErrExcluded) {
		t.Errorf("err=%v; want ErrExcluded", err)
	}
}

func TestMountResolverLabelsSorted(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("charlie", "/c", false, nil)
	r.Register("alpha", "/a", false, nil)
	r.Register("bravo", "/b", false, nil)
	labels := r.Labels()
	if strings.Join(labels, ",") != "alpha,bravo,charlie" {
		t.Errorf("labels order: %v", labels)
	}
}
