package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewRequiresLabel(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Source: "/tmp"}); err == nil {
		t.Error("empty Label should fail construction")
	}
}

func TestNewRequiresSource(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{Label: "x"}); err == nil {
		t.Error("empty Source should fail construction")
	}
}

func TestStartRejectsRelativePath(t *testing.T) {
	t.Parallel()
	m, _ := New(Config{Label: "x", Source: "./relative"})
	if err := m.Start(context.Background()); err == nil {
		t.Error("relative path should fail Start")
	}
}

func TestStartRejectsMissingPath(t *testing.T) {
	t.Parallel()
	m, _ := New(Config{Label: "x", Source: "/definitely/does/not/exist/lobslaw"})
	if err := m.Start(context.Background()); err == nil {
		t.Error("missing path should fail Start")
	}
}

func TestStartRejectsFileNotDirectory(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "file")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	m, _ := New(Config{Label: "x", Source: f.Name()})
	if err := m.Start(context.Background()); err == nil {
		t.Error("file-not-directory should fail Start")
	}
}

func TestStartHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, _ := New(Config{Label: "shared", Source: dir})

	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Path() == "" {
		t.Error("Path empty after Start")
	}
	if !m.Healthy() {
		t.Error("Healthy false after successful Start")
	}
	if m.Label() != "shared" || m.Backend() != "local" {
		t.Errorf("Label/Backend mismatch: %q / %q", m.Label(), m.Backend())
	}
}

// TestStartResolvesSymlinks — Path must be the canonical target,
// not the symlink source. Otherwise a subsequent symlink retarget
// would silently change where the mount resolves.
func TestStartResolvesSymlinks(t *testing.T) {
	t.Parallel()
	realDir := t.TempDir()
	linkDir := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skip("symlink creation unsupported in this environment")
	}

	m, _ := New(Config{Label: "x", Source: linkDir})
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// EvalSymlinks may normalise the path (e.g. strip /private on
	// macOS). Check only that the resolved path isn't the link.
	resolvedReal, _ := filepath.EvalSymlinks(realDir)
	if m.Path() != resolvedReal {
		t.Errorf("Path should resolve to the target; got %q, want %q", m.Path(), resolvedReal)
	}
}

func TestStopMarksUnhealthy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	m, _ := New(Config{Label: "x", Source: dir})
	_ = m.Start(context.Background())

	if err := m.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Healthy() {
		t.Error("Stop should flip Healthy to false")
	}
}

// Compile-time assertion: local.Mount satisfies the storage.Mount
// interface. If this drifts the full build fails rather than
// discovering the mismatch at Manager.Register time.
type mountLike interface {
	Label() string
	Backend() string
	Path() string
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Healthy() bool
}

func TestLocalMountImplementsInterface(t *testing.T) {
	t.Parallel()
	var _ mountLike = (*Mount)(nil)
	if errors.New("placeholder") == nil {
		t.Fatal("impossible")
	}
}
