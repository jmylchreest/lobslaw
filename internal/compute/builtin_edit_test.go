package compute

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	out, exit, err := writeFileBuiltin(context.Background(), map[string]string{
		"path":    path,
		"content": "hello lobslaw",
	})
	if err != nil || exit != 0 {
		t.Fatalf("err=%v exit=%d", err, exit)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "hello lobslaw" {
		t.Errorf("file contents = %q", raw)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["bytes"] != float64(13) {
		t.Errorf("bytes = %v; want 13", resp["bytes"])
	}
}

func TestWriteFileRejectsRelative(t *testing.T) {
	t.Parallel()
	_, _, err := writeFileBuiltin(context.Background(), map[string]string{
		"path":    "x.txt",
		"content": "hi",
	})
	if err == nil {
		t.Error("relative path should fail")
	}
}

func TestWriteFileRejectsOversize(t *testing.T) {
	t.Parallel()
	_, _, err := writeFileBuiltin(context.Background(), map[string]string{
		"path":    "/tmp/oversize.txt",
		"content": strings.Repeat("x", writeFileMaxBytes+1),
	})
	if err == nil {
		t.Error("oversized content should be rejected")
	}
}

func TestEditFileReplaceSingle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "doc.md")
	_ = os.WriteFile(path, []byte("Hello world\nGoodbye"), 0o644)

	out, exit, err := editFileBuiltin(context.Background(), map[string]string{
		"path":       path,
		"old_string": "Hello",
		"new_string": "Greetings",
	})
	if err != nil || exit != 0 {
		t.Fatalf("err=%v exit=%d", err, exit)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "Greetings world\nGoodbye" {
		t.Errorf("file = %q", raw)
	}
	var resp map[string]any
	_ = json.Unmarshal(out, &resp)
	if resp["replacements"] != float64(1) {
		t.Errorf("replacements = %v", resp["replacements"])
	}
}

func TestEditFileAmbiguousWithoutReplaceAll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.txt")
	_ = os.WriteFile(path, []byte("foo foo foo"), 0o644)
	_, exit, err := editFileBuiltin(context.Background(), map[string]string{
		"path":       path,
		"old_string": "foo",
		"new_string": "bar",
	})
	if err == nil || exit == 0 {
		t.Error("multi-match without replace_all should fail")
	}
}

func TestEditFileReplaceAll(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dup.txt")
	_ = os.WriteFile(path, []byte("foo foo foo"), 0o644)
	_, _, err := editFileBuiltin(context.Background(), map[string]string{
		"path":        path,
		"old_string":  "foo",
		"new_string":  "bar",
		"replace_all": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "bar bar bar" {
		t.Errorf("replace_all result = %q", raw)
	}
}

func TestEditFileNoMatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.txt")
	_ = os.WriteFile(path, []byte("content"), 0o644)
	_, exit, err := editFileBuiltin(context.Background(), map[string]string{
		"path":       path,
		"old_string": "missing",
		"new_string": "present",
	})
	if err == nil || exit == 0 {
		t.Error("no-match should fail")
	}
}

func TestEditFileRejectsNoopReplace(t *testing.T) {
	t.Parallel()
	_, _, err := editFileBuiltin(context.Background(), map[string]string{
		"path":       "/tmp/x",
		"old_string": "same",
		"new_string": "same",
	})
	if err == nil {
		t.Error("same old/new should fail")
	}
}
