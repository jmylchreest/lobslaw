package promptgen

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestLoadBootstrapEmptyConfig(t *testing.T) {
	t.Parallel()
	res := LoadBootstrap(BootstrapConfig{})
	if res.Body != "" || len(res.Loaded) != 0 || len(res.Skipped) != 0 {
		t.Errorf("empty config should yield empty result; got %+v", res)
	}
}

func TestLoadBootstrapInOrder(t *testing.T) {
	t.Parallel()
	res := LoadBootstrap(BootstrapConfig{
		Paths: []string{"first", "second", "third"},
		FS: MapFS{
			"first":  "alpha",
			"second": "beta",
			"third":  "gamma",
		},
	})
	if len(res.Loaded) != 3 {
		t.Fatalf("expected 3 loaded; got %d", len(res.Loaded))
	}
	// Body must contain all three in the declared order.
	posAlpha := strings.Index(res.Body, "alpha")
	posBeta := strings.Index(res.Body, "beta")
	posGamma := strings.Index(res.Body, "gamma")
	if !(posAlpha < posBeta && posBeta < posGamma) {
		t.Errorf("bootstrap order violated: body=%q", res.Body)
	}
}

func TestLoadBootstrapPerFileCapTruncates(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 1000)
	res := LoadBootstrap(BootstrapConfig{
		Paths:           []string{"big"},
		MaxCharsPerFile: 100,
		FS:              MapFS{"big": big},
	})
	if !slices.Contains(res.Truncated, "big") {
		t.Errorf("big file should be truncated; got %+v", res)
	}
	if !strings.Contains(res.Body, "truncated per-file") {
		t.Errorf("truncation sentinel missing: %q", res.Body)
	}
	// Body length should be close to cap + sentinel — not the full 1000.
	if len(res.Body) > 400 {
		t.Errorf("body unexpectedly large (%d bytes); cap should have held", len(res.Body))
	}
}

func TestLoadBootstrapTotalCapTruncatesTail(t *testing.T) {
	t.Parallel()
	res := LoadBootstrap(BootstrapConfig{
		Paths:         []string{"small", "mid", "tail"},
		MaxTotalChars: 150,
		FS: MapFS{
			"small": strings.Repeat("s", 20),
			"mid":   strings.Repeat("m", 60),
			"tail":  strings.Repeat("t", 500),
		},
	})
	// First two should load whole; tail should either truncate or skip.
	if len(res.Loaded) < 2 {
		t.Errorf("small + mid should have loaded; got %+v", res.Loaded)
	}
	body := res.Body
	if len(body) > 200 {
		t.Errorf("total cap blown; body=%d", len(body))
	}
}

func TestLoadBootstrapSkipAfterTotalCap(t *testing.T) {
	t.Parallel()
	res := LoadBootstrap(BootstrapConfig{
		Paths:         []string{"hog", "remainder"},
		MaxTotalChars: 100,
		FS: MapFS{
			"hog":       strings.Repeat("h", 500),
			"remainder": "tiny",
		},
	})
	// hog truncates to fit; remainder should skip with "total cap reached".
	if !slices.ContainsFunc(res.Skipped, func(s BootstrapSkipped) bool {
		return s.Path == "remainder" && strings.Contains(s.Reason, "total cap")
	}) {
		t.Errorf("expected 'remainder' to be skipped with cap-reason; got %+v", res.Skipped)
	}
}

func TestLoadBootstrapReadErrorDoesNotHaltLoad(t *testing.T) {
	t.Parallel()
	res := LoadBootstrap(BootstrapConfig{
		Paths: []string{"missing", "present"},
		FS:    MapFS{"present": "payload"},
	})
	// missing → skipped with read-error reason.
	if !slices.ContainsFunc(res.Skipped, func(s BootstrapSkipped) bool {
		return s.Path == "missing" && strings.Contains(s.Reason, "read")
	}) {
		t.Errorf("missing file should be in Skipped; got %+v", res.Skipped)
	}
	// present → still loads despite sibling's failure.
	if !strings.Contains(res.Body, "payload") {
		t.Error("sibling's read error should not block the rest")
	}
}

func TestLoadBootstrapDefaultFSReadsRealDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.md")
	if err := os.WriteFile(path, []byte("from disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := LoadBootstrap(BootstrapConfig{Paths: []string{path}})
	if !strings.Contains(res.Body, "from disk") {
		t.Errorf("default FS should read real disk; body=%q", res.Body)
	}
}

func TestLoadBootstrapIncludesPathHeaders(t *testing.T) {
	t.Parallel()
	res := LoadBootstrap(BootstrapConfig{
		Paths: []string{"/cfg/a.md", "/cfg/b.md"},
		FS: MapFS{
			"/cfg/a.md": "alpha",
			"/cfg/b.md": "beta",
		},
	})
	// Each file gets a `<!-- bootstrap: path -->` heading so the
	// model can attribute content back to its source.
	if !strings.Contains(res.Body, "<!-- bootstrap: /cfg/a.md -->") {
		t.Errorf("path header missing: %q", res.Body)
	}
	if !strings.Contains(res.Body, "<!-- bootstrap: /cfg/b.md -->") {
		t.Errorf("path header missing: %q", res.Body)
	}
}

func TestLoadBootstrapCountsBytesPerFile(t *testing.T) {
	t.Parallel()
	res := LoadBootstrap(BootstrapConfig{
		Paths: []string{"p"},
		FS:    MapFS{"p": "hello"},
	})
	if len(res.Loaded) != 1 {
		t.Fatalf("expected 1 loaded; got %d", len(res.Loaded))
	}
	if res.Loaded[0].Path != "p" {
		t.Errorf("path: %q", res.Loaded[0].Path)
	}
	if res.Loaded[0].Bytes <= 5 {
		t.Errorf("bytes should include the heading + content (>5); got %d", res.Loaded[0].Bytes)
	}
}

func TestLoadBootstrapZeroCapsMeanUnlimited(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 10000)
	res := LoadBootstrap(BootstrapConfig{
		Paths: []string{"big"},
		// Both caps = 0 → unlimited.
		FS: MapFS{"big": big},
	})
	if len(res.Truncated) != 0 {
		t.Errorf("zero caps should NOT truncate; got truncated=%v", res.Truncated)
	}
	if !strings.Contains(res.Body, big) {
		t.Error("full content should be present")
	}
}
