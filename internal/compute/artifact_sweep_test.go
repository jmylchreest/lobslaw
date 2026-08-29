package compute

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeAged puts a file in generated/ with a known size and mtime, so a
// test can state the order the sweep should remove them in.
func writeAged(t *testing.T, root, name string, size int, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, generatedDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return p
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// The sweep removes OLDEST FIRST and stops as soon as the budget is
// met, rather than clearing the directory.
func TestSweepRemovesOldestUntilUnderBudget(t *testing.T) {
	root := t.TempDir()
	oldest := writeAged(t, root, "oldest.mp4", 4096, 72*time.Hour)
	middle := writeAged(t, root, "middle.mp4", 4096, 48*time.Hour)
	newest := writeAged(t, root, "newest.mp4", 4096, time.Hour)

	// A budget that fits roughly two of the three. The exact percentage
	// is derived from the real filesystem, so the test asks for a
	// budget in bytes and converts.
	total, err := filesystemBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	pct := float64(9000) / float64(total) * 100

	sweepGenerated(root, ArtifactRetention{DiskPercent: pct, Logger: quiet()})

	if exists(oldest) {
		t.Error("the oldest file survived a sweep that had to free space")
	}
	if !exists(middle) || !exists(newest) {
		t.Error("the sweep removed more than it needed to")
	}
}

// Under budget is a no-op. A sweep that deletes anything while there is
// room is a sweep that loses a file for nothing.
func TestSweepIsANoOpUnderBudget(t *testing.T) {
	root := t.TempDir()
	keep := writeAged(t, root, "small.mp3", 1024, time.Hour)

	sweepGenerated(root, ArtifactRetention{DiskPercent: 50, Logger: quiet()})

	if !exists(keep) {
		t.Error("a file was removed while under budget")
	}
}

// A negative percentage is how an operator says "something else manages
// this mount". It has to mean nothing is touched, whatever the usage.
func TestSweepDisabledByNegativePercent(t *testing.T) {
	root := t.TempDir()
	big := writeAged(t, root, "huge.mp4", 65536, 99*time.Hour)

	sweepGenerated(root, ArtifactRetention{DiskPercent: -1, Logger: quiet()})

	if !exists(big) {
		t.Error("sweeping ran despite being disabled")
	}
}

// The sweep must never be able to reach the rest of the mount. The same
// tree carries the operator's workspace, inbound attachments, and the
// export directory that #200 will use to mean "keep this".
func TestSweepTouchesNothingOutsideGenerated(t *testing.T) {
	root := t.TempDir()
	writeAged(t, root, "fill-a.mp4", 32768, 80*time.Hour)
	writeAged(t, root, "fill-b.mp4", 32768, 79*time.Hour)

	for _, sibling := range []string{"export", "incoming", "state"} {
		d := filepath.Join(root, sibling)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "keep.bin"), make([]byte, 32768), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	total, _ := filesystemBytes(root)
	sweepGenerated(root, ArtifactRetention{
		DiskPercent: float64(1) / float64(total) * 100, // effectively zero: sweep everything it may
		Logger:      quiet(),
	})

	for _, sibling := range []string{"export", "incoming", "state"} {
		if !exists(filepath.Join(root, sibling, "keep.bin")) {
			t.Errorf("the sweep removed a file in %s/, outside generated/", sibling)
		}
	}
}

// A node that has generated nothing has nothing to sweep, and must not
// report that as a failure.
func TestSweepToleratesMissingGeneratedDir(t *testing.T) {
	sweepGenerated(t.TempDir(), ArtifactRetention{DiskPercent: 5, Logger: quiet()})
}

// The budget is wanted before the first file is written, so sizing has
// to work on a directory that does not exist yet.
func TestFilesystemBytesClimbsToAnExistingAncestor(t *testing.T) {
	root := t.TempDir()
	got, err := filesystemBytes(filepath.Join(root, "generated", "not", "yet"))
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 {
		t.Errorf("filesystem size = %d", got)
	}
}

// Writing an artifact is what bounds the directory; there is no
// scheduler involved.
func TestResolverSweepsAfterWriting(t *testing.T) {
	root := t.TempDir()
	old := writeAged(t, root, "old.mp4", 8192, 96*time.Hour)

	total, _ := filesystemBytes(root)
	r := &ArtifactResolver{
		Mounts:       fixedMount{root: root},
		DefaultMount: "workspace",
		Retention: ArtifactRetention{
			DiskPercent: float64(5000) / float64(total) * 100,
			Logger:      quiet(),
		},
	}
	got, err := r.write("fresh", "audio/mpeg", make([]byte, 4096))
	if err != nil {
		t.Fatal(err)
	}
	if exists(old) {
		t.Error("the older file survived a write that pushed the directory over budget")
	}
	// The file just written is the newest, so it is never the candidate.
	if !exists(filepath.Join(root, got.Path)) {
		t.Error("the sweep removed the artifact that triggered it")
	}
}

// fixedMount is a MountWriter with one root, for tests that only care
// about the filesystem side.
type fixedMount struct{ root string }

func (m fixedMount) MountRoot(string) (string, bool) { return m.root, true }
