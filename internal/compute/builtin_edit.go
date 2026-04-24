package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// writeFileMaxBytes caps the content we'll write in a single call.
// 1MB is plenty for any reasonable text file the model might author
// and prevents a runaway completion from filling a disk.
const writeFileMaxBytes = 1 * 1024 * 1024

// fileEditLocks serialises concurrent edits against the same path.
// Without it a double-tool-call on the same file could race and
// produce half-applied edits. Opencode uses the same pattern.
var fileEditLocks = struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}{locks: map[string]*sync.Mutex{}}

func lockForPath(path string) *sync.Mutex {
	fileEditLocks.mu.Lock()
	defer fileEditLocks.mu.Unlock()
	if l, ok := fileEditLocks.locks[path]; ok {
		return l
	}
	l := &sync.Mutex{}
	fileEditLocks.locks[path] = l
	return l
}

// WriteToolDef / EditToolDef are separate from StdlibToolDefs
// because their RiskTier is stricter (irreversible file mutation).
// Policy defaults to ask-on-first-write, cached via the permission
// rule store — identical shape to opencode's "once"/"always" ruleset.
func WriteToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "write_file",
		Path:        BuiltinScheme + "write_file",
		Description: "Create or overwrite a text file at the given absolute path. Use sparingly — this is destructive, overwrites existing content, and doesn't offer diff preview. Prefer edit_file for changes to existing files. content is the full new file body; path must be absolute.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute filesystem path."},
				"content": {"type": "string", "description": "Full file body to write."}
			},
			"required": ["path", "content"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskIrreversible,
	}
}

func EditToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "edit_file",
		Path:        BuiltinScheme + "edit_file",
		Description: "Replace one specific substring in a file with another. old_string must match exactly once in the file (whitespace and indentation count). new_string replaces it. Use replace_all=true when every occurrence should change. Prefer this over write_file when making small changes to preserve unchanged content.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"path": {"type": "string", "description": "Absolute filesystem path."},
				"old_string": {"type": "string", "description": "Substring to find (must be unique unless replace_all)."},
				"new_string": {"type": "string", "description": "Replacement text."},
				"replace_all": {"type": "boolean", "description": "Replace every occurrence."}
			},
			"required": ["path", "old_string", "new_string"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskIrreversible,
	}
}

// RegisterWriteEditBuiltins installs write_file + edit_file. Always
// registered (no secret dependency); policy gates whether a given
// scope may call them.
func RegisterWriteEditBuiltins(b *Builtins) error {
	if err := b.Register("write_file", writeFileBuiltin); err != nil {
		return err
	}
	return b.Register("edit_file", editFileBuiltin)
}

// writeFileBuiltin writes the exact content bytes to path. Replaces
// existing content. The file's parent directory must already exist
// — we don't MkdirAll, because "I meant to save to ~/notes but
// typo'd" silently creating directory trees is worse than a
// descriptive error.
func writeFileBuiltin(_ context.Context, args map[string]string) ([]byte, int, error) {
	path := strings.TrimSpace(args["path"])
	if path == "" {
		return nil, 2, errors.New("write_file: path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, 2, errors.New("write_file: path must be absolute")
	}
	content := args["content"]
	if len(content) > writeFileMaxBytes {
		return nil, 2, fmt.Errorf("write_file: content exceeds %d bytes", writeFileMaxBytes)
	}

	lock := lockForPath(path)
	lock.Lock()
	defer lock.Unlock()

	// If the parent directory is missing, the write fails with a
	// clear error — caller (or user) can act on it.
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return nil, 1, fmt.Errorf("write_file: %w", err)
	}
	out, _ := json.Marshal(map[string]any{
		"path":  path,
		"bytes": len(content),
	})
	return out, 0, nil
}

// editFileBuiltin reads path, replaces old_string with new_string,
// writes back. When replace_all is false (the default), old_string
// must appear exactly once — the "I meant the first one" class of
// bug is ruled out. The file's per-path lock prevents interleaved
// edits producing inconsistent state.
func editFileBuiltin(_ context.Context, args map[string]string) ([]byte, int, error) {
	path := strings.TrimSpace(args["path"])
	if path == "" {
		return nil, 2, errors.New("edit_file: path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, 2, errors.New("edit_file: path must be absolute")
	}
	oldStr := args["old_string"]
	newStr := args["new_string"]
	if oldStr == "" {
		return nil, 2, errors.New("edit_file: old_string is required")
	}
	if oldStr == newStr {
		return nil, 2, errors.New("edit_file: old_string equals new_string; nothing to change")
	}
	replaceAll := args["replace_all"] == "true"

	lock := lockForPath(path)
	lock.Lock()
	defer lock.Unlock()

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, 1, fmt.Errorf("edit_file: read: %w", err)
	}
	bodyStr := string(body)
	count := strings.Count(bodyStr, oldStr)
	if count == 0 {
		return nil, 2, fmt.Errorf("edit_file: old_string not found in %s", path)
	}
	if !replaceAll && count > 1 {
		return nil, 2, fmt.Errorf("edit_file: old_string matches %d times; pass replace_all=true or give a more specific substring", count)
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(bodyStr, oldStr, newStr)
	} else {
		updated = strings.Replace(bodyStr, oldStr, newStr, 1)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return nil, 1, fmt.Errorf("edit_file: write: %w", err)
	}

	out, _ := json.Marshal(map[string]any{
		"path":         path,
		"replacements": count,
		"replace_all":  replaceAll,
	})
	return out, 0, nil
}
