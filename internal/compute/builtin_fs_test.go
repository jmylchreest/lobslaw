package compute

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadFileHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644)

	out, exit, err := readFileBuiltin(context.Background(), map[string]string{"path": path})
	if err != nil || exit != 0 {
		t.Fatalf("err=%v exit=%d", err, exit)
	}
	var payload struct {
		LineCount int    `json:"line_count"`
		Content   string `json:"content"`
	}
	_ = json.Unmarshal(out, &payload)
	if payload.LineCount != 4 {
		t.Errorf("line_count = %d; want 4", payload.LineCount)
	}
	if !strings.Contains(payload.Content, "three") {
		t.Errorf("content missing 'three': %q", payload.Content)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(path, []byte("a\nb\nc\nd\ne\n"), 0o644)

	out, _, _ := readFileBuiltin(context.Background(), map[string]string{
		"path":   path,
		"offset": "2",
		"limit":  "2",
	})
	var payload struct {
		Content  string `json:"content"`
		Returned int    `json:"returned"`
	}
	_ = json.Unmarshal(out, &payload)
	if payload.Returned != 2 {
		t.Errorf("returned = %d; want 2", payload.Returned)
	}
	if payload.Content != "c\nd" {
		t.Errorf("content = %q; want \"c\\nd\"", payload.Content)
	}
}

func TestReadFileRejectsRelativePath(t *testing.T) {
	t.Parallel()
	_, exit, _ := readFileBuiltin(context.Background(), map[string]string{"path": "relative.txt"})
	if exit == 0 {
		t.Error("relative path should produce a tool error (exit != 0)")
	}
}

func TestReadFileRejectsMissing(t *testing.T) {
	t.Parallel()
	_, exit, _ := readFileBuiltin(context.Background(), map[string]string{"path": "/does/not/exist"})
	if exit == 0 {
		t.Error("missing file should produce a tool error (exit != 0)")
	}
}

func TestSearchFilesMatchesAndEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello world\nlobslaw\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("unrelated\n"), 0o644)

	out, exit, err := searchFilesBuiltin(context.Background(), map[string]string{
		"pattern": "lobslaw",
		"path":    dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Errorf("exit = %d", exit)
	}
	var payload struct {
		Matches []map[string]any `json:"matches"`
	}
	_ = json.Unmarshal(out, &payload)
	if len(payload.Matches) != 1 {
		t.Errorf("matches = %d; want 1: %+v", len(payload.Matches), payload.Matches)
	}

	// No-match → empty matches + exit 0.
	out, exit, err = searchFilesBuiltin(context.Background(), map[string]string{
		"pattern": "zznomatchzz",
		"path":    dir,
	})
	if err != nil || exit != 0 {
		t.Fatalf("no-match err=%v exit=%d", err, exit)
	}
	_ = json.Unmarshal(out, &payload)
	if len(payload.Matches) != 0 {
		t.Errorf("no-match should be empty; got %d", len(payload.Matches))
	}
}

func TestSearchFilesRejectsEmptyPattern(t *testing.T) {
	t.Parallel()
	_, exit, _ := searchFilesBuiltin(context.Background(), map[string]string{})
	if exit == 0 {
		t.Error("empty pattern should produce a tool error (exit != 0)")
	}
}
