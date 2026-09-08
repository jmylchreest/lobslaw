package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// --- fixtures ---------------------------------------------------------

// exportBuiltin wires the export tool against a fresh temp mount and
// returns the handler plus the mount root, so a test can write a
// source under generated/ and inspect export/ afterwards.
func exportBuiltin(t *testing.T) (compute.BuiltinFunc, string) {
	t.Helper()
	root := t.TempDir()
	b := NewBuiltins()
	resolver := &compute.ArtifactResolver{
		Mounts:       fakeMounts{label: "store", root: root},
		DefaultMount: "store",
	}
	if err := RegisterExportBuiltin(b, ExportConfig{Resolver: resolver}); err != nil {
		t.Fatal(err)
	}
	h, ok := b.Get("export")
	if !ok {
		t.Fatal("export not registered")
	}
	return h, root
}

// writeGenerated drops a source file under the mount's generated/,
// creating the directory if this is the first file in it.
func writeGenerated(t *testing.T, root, name string, content []byte) string {
	t.Helper()
	dir := filepath.Join(root, compute.GeneratedDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type exportResult struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// --- happy path ---------------------------------------------------------

func TestExportCopiesFileToExportDir(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)
	content := []byte("fake mp4 bytes")
	writeGenerated(t, root, "clip.mp4", content)

	out, code, err := h(context.Background(), map[string]string{"path": "generated/clip.mp4"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v (%s)", code, err, out)
	}
	var got exportResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v (%s)", err, out)
	}
	if got.Path != "export/clip.mp4" {
		t.Errorf("path = %q, want export/clip.mp4", got.Path)
	}
	if got.Bytes != int64(len(content)) {
		t.Errorf("bytes = %d, want %d", got.Bytes, len(content))
	}
	copied, err := os.ReadFile(filepath.Join(root, "export", "clip.mp4"))
	if err != nil {
		t.Fatalf("copy not written: %v", err)
	}
	if !bytes.Equal(copied, content) {
		t.Error("copied content does not match the source")
	}
}

// Copy, not move: retention still owns the original until IT decides
// to remove it. A move here would break a delivery still in flight on
// a channel that has not attached the file yet.
func TestExportOriginalSurvives(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)
	content := []byte("original bytes, unmoved")
	src := writeGenerated(t, root, "keep.mp4", content)

	if _, code, err := h(context.Background(), map[string]string{"path": "generated/keep.mp4"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("source no longer exists: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Error("source content changed by the export")
	}
}

// The JSON body the model reads is exactly {"path", "bytes"} — nothing
// else, matching the shape #200 specifies. Extra fields (mount, mime)
// belong on the CollectArtifact announcement, not in what the model
// parses.
func TestExportReturnsPathAndBytes(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)
	writeGenerated(t, root, "clip.mp4", []byte("bytes-bytes-bytes"))

	out, code, err := h(context.Background(), map[string]string{"path": "generated/clip.mp4"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(out, &raw); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if len(raw) != 2 {
		t.Errorf("result has %d top-level fields, want exactly path and bytes: %s", len(raw), out)
	}
	if _, ok := raw["path"]; !ok {
		t.Error(`result missing "path"`)
	}
	if _, ok := raw["bytes"]; !ok {
		t.Error(`result missing "bytes"`)
	}
}

func TestExportCreatesExportDir(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)
	if _, err := os.Stat(filepath.Join(root, "export")); !os.IsNotExist(err) {
		t.Fatal("export/ already exists before the first export")
	}
	writeGenerated(t, root, "clip.mp4", []byte("x"))
	if _, code, err := h(context.Background(), map[string]string{"path": "generated/clip.mp4"}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if _, err := os.Stat(filepath.Join(root, "export")); err != nil {
		t.Error("export/ was not created")
	}
}

// --- collisions -----------------------------------------------------

// A second source that happens to share a destination name gets a
// numbered suffix, and the first export is never touched.
func TestExportCollisionSuffix(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)
	writeGenerated(t, root, "a.mp4", []byte("first"))
	if _, code, err := h(context.Background(), map[string]string{"path": "generated/a.mp4"}); err != nil || code != 0 {
		t.Fatalf("first export: code=%d err=%v", code, err)
	}

	// A DIFFERENT file, in a different source directory, that happens
	// to end in the same basename "a.mp4".
	otherDir := filepath.Join(root, compute.GeneratedDir, "other")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "a.mp4"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, code, err := h(context.Background(), map[string]string{"path": "generated/other/a.mp4"})
	if err != nil || code != 0 {
		t.Fatalf("second export: code=%d err=%v", code, err)
	}
	var got exportResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if got.Path != "export/a_1.mp4" {
		t.Errorf("path = %q, want export/a_1.mp4", got.Path)
	}

	first, err := os.ReadFile(filepath.Join(root, "export", "a.mp4"))
	if err != nil || string(first) != "first" {
		t.Errorf("the first export was overwritten: content=%q err=%v", first, err)
	}
	second, err := os.ReadFile(filepath.Join(root, "export", "a_1.mp4"))
	if err != nil || string(second) != "second" {
		t.Errorf("the second export landed wrong: content=%q err=%v", second, err)
	}
}

// Re-exporting the SAME content under the SAME name is a no-op that
// returns the existing path rather than writing a_1.mp4 — the whole
// point of dedup is that a repeated export call is safe to repeat.
func TestExportCollisionIdempotent(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)
	writeGenerated(t, root, "a.mp4", []byte("identical bytes"))

	first, code, err := h(context.Background(), map[string]string{"path": "generated/a.mp4"})
	if err != nil || code != 0 {
		t.Fatalf("first export: code=%d err=%v", code, err)
	}
	second, code, err := h(context.Background(), map[string]string{"path": "generated/a.mp4"})
	if err != nil || code != 0 {
		t.Fatalf("second export: code=%d err=%v", code, err)
	}
	if string(first) != string(second) {
		t.Errorf("re-exporting identical content produced a different result:\nfirst:  %s\nsecond: %s", first, second)
	}
	entries, err := os.ReadDir(filepath.Join(root, "export"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("export/ has %d entries %v, want exactly 1 (dedup should not write a second copy)", len(entries), names)
	}
}

// --- refusals -----------------------------------------------------

// The path argument comes from the model. Every shape of traversal
// must be refused rather than resolved.
//
// Each case (other than the absolute one, which targets the real
// /etc/passwd already on the test machine) plants a REAL file exactly
// where the traversal would land if the confinement did nothing at
// all. That matters: most of these strings resolve to a location that
// would not otherwise exist, so an unguarded export would still fail
// with "source not found" and a test that only checked for a nonzero
// exit code would pass whether or not the confinement was doing
// anything. Planting the target and then checking export/ never ends
// up holding its content is what actually proves the escape was
// stopped rather than merely coincidental.
func TestExportTraversalRefused(t *testing.T) {
	t.Parallel()

	const planted = "outside the mount; must never be copied"
	cases := []struct {
		name   string
		path   string
		target func(mountParent string) string // where an unguarded export would read from
	}{
		{
			name:   "dot-dot form",
			path:   "../escaped-a.mp4",
			target: func(p string) string { return filepath.Join(p, "escaped-a.mp4") },
		},
		{
			// Clean folds "generated/../.." to a single "..", landing at
			// the same depth as the case above — the point is that the
			// escape survives an intermediate segment, not that it goes
			// up two levels.
			name:   "generated then double dot-dot",
			path:   "generated/../../escaped-b.mp4",
			target: func(p string) string { return filepath.Join(p, "escaped-b.mp4") },
		},
		{
			name:   "dot-dot into a sibling export",
			path:   "../export/escaped-c.mp4",
			target: func(p string) string { return filepath.Join(p, "export", "escaped-c.mp4") },
		},
		{
			name: "absolute path",
			path: "/etc/passwd",
			// Real on every Unix test runner; nothing to plant, and its
			// existing content is exactly the point — it must never show
			// up under export/.
			target: func(string) string { return "/etc/passwd" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, root := exportBuiltin(t)
			writeGenerated(t, root, "clip.mp4", []byte("in-mount, unrelated to the attack"))

			target := tc.target(filepath.Dir(root))
			if tc.path != "/etc/passwd" {
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(planted), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			out, code, err := h(context.Background(), map[string]string{"path": tc.path})
			if code == 0 {
				t.Fatalf("path %q was accepted (out=%s)", tc.path, out)
			}
			if err != nil {
				t.Fatalf("expected a structured JSON error, got a Go error: %v", err)
			}
			entries, _ := os.ReadDir(filepath.Join(root, "export"))
			for _, e := range entries {
				b, _ := os.ReadFile(filepath.Join(root, "export", e.Name()))
				if string(b) == planted {
					t.Fatalf("path %q escaped the mount: export/%s holds the outside file's content", tc.path, e.Name())
				}
			}
		})
	}
}

// A symlink under generated/ pointing outside the mount would let the
// copy escape confinement entirely if export followed it. Neither
// safeArtifactName nor artifactOpener checks this; export has to be
// the first to.
//
// The refusal itself would happen anyway — a symlink's own Lstat mode
// is never "regular", so the generic not-a-file branch would catch it
// even without a dedicated case. What only the dedicated case gives is
// the specific reason: "symlink_refused" rather than the generic
// "not_a_file". Asserting on the error_type is what makes removing the
// dedicated branch actually fail this test.
func TestExportSymlinkRefused(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)

	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not copy me"), 0o600); err != nil {
		t.Fatal(err)
	}
	genDir := filepath.Join(root, compute.GeneratedDir)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(genDir, "escape.mp4")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}

	out, code, err := h(context.Background(), map[string]string{"path": "generated/escape.mp4"})
	if code == 0 {
		t.Fatalf("a symlink escaping the mount was exported (out=%s)", out)
	}
	if err != nil {
		t.Fatalf("expected a structured JSON error, got a Go error: %v", err)
	}
	var toolErr struct {
		ErrorType string `json:"error_type"`
	}
	if jsonErr := json.Unmarshal(out, &toolErr); jsonErr != nil {
		t.Fatalf("result is not JSON: %v (%s)", jsonErr, out)
	}
	if toolErr.ErrorType != "symlink_refused" {
		t.Errorf("error_type = %q, want \"symlink_refused\" (got a generic refusal instead of the symlink-specific one)", toolErr.ErrorType)
	}
	if _, statErr := os.Stat(filepath.Join(root, "export", "escape.mp4")); statErr == nil {
		t.Error("the symlink's target was copied into export/")
	}
}

func TestExportSourceMustExist(t *testing.T) {
	t.Parallel()
	h, _ := exportBuiltin(t)
	out, code, err := h(context.Background(), map[string]string{"path": "generated/nope.mp4"})
	if code == 0 {
		t.Fatalf("a nonexistent source exported successfully (out=%s)", out)
	}
	if err != nil {
		t.Fatalf("expected a structured JSON error, got a Go error: %v", err)
	}
}

func TestExportSourceMustBeRegularFile(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)
	dir := filepath.Join(root, compute.GeneratedDir, "adir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code, err := h(context.Background(), map[string]string{"path": "generated/adir"})
	if code == 0 {
		t.Fatalf("a directory was exported as though it were a file (out=%s)", out)
	}
	if err != nil {
		t.Fatalf("expected a structured JSON error, got a Go error: %v", err)
	}
}

// Confinement is to the MOUNT ROOT, not merely to a syntactic "..".
// A real file that exists just outside the mount, reachable only via
// traversal, must still be refused.
func TestExportConfinedToMount(t *testing.T) {
	t.Parallel()
	h, root := exportBuiltin(t)

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "real.mp4"), []byte("real file, wrong mount"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(root, filepath.Join(outside, "real.mp4"))
	if err != nil {
		t.Fatal(err)
	}

	out, code, hErr := h(context.Background(), map[string]string{"path": rel})
	if code == 0 {
		t.Fatalf("a file outside the mount (%q) was exported (out=%s)", rel, out)
	}
	if hErr != nil {
		t.Fatalf("expected a structured JSON error, got a Go error: %v", hErr)
	}
}

func TestExportRequiresPath(t *testing.T) {
	t.Parallel()
	h, _ := exportBuiltin(t)
	if _, code, err := h(context.Background(), map[string]string{}); code == 0 || err != nil {
		t.Errorf("missing path: code=%d err=%v, want a structured argument error", code, err)
	}
}

// A resolver-less export could never find a mount to copy from or to,
// so registration fails at boot rather than at the first call.
func TestExportRequiresAResolver(t *testing.T) {
	t.Parallel()
	if err := RegisterExportBuiltin(NewBuiltins(), ExportConfig{}); err == nil {
		t.Error("registered an export tool with no artifact resolver")
	}
}
