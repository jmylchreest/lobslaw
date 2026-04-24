package compute

import (
	"context"
	"encoding/json"
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
		return marshalToolError("missing_arg", "path is required", "pass an absolute filesystem path")
	}
	if !filepath.IsAbs(path) {
		return marshalToolError("relative_path", "path must be absolute",
			"prefix with / (e.g. /home/johnm/lobslaw/notes.md)")
	}
	if isInternalPath(path) {
		return marshalToolError("internal_path", path+" is cluster-internal and cannot be written",
			"pick a path inside a configured storage mount")
	}
	content := args["content"]
	if len(content) > writeFileMaxBytes {
		return marshalToolError("content_too_large",
			fmt.Sprintf("content is %d bytes, max %d", len(content), writeFileMaxBytes),
			"split the write into multiple smaller files or truncate")
	}

	lock := lockForPath(path)
	lock.Lock()
	defer lock.Unlock()

	// If the parent directory is missing, the write fails with a
	// clear error — caller (or user) can act on it.
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		if os.IsNotExist(err) {
			return marshalToolError("parent_not_found", err.Error(),
				"the parent directory doesn't exist. Create it first or pick an existing directory")
		}
		if os.IsPermission(err) {
			return marshalToolError("permission_denied", err.Error(), "")
		}
		return marshalToolError("write_failed", err.Error(), "")
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
		return marshalToolError("missing_arg", "path is required", "pass an absolute filesystem path")
	}
	if !filepath.IsAbs(path) {
		return marshalToolError("relative_path", "path must be absolute", "prefix with /")
	}
	if isInternalPath(path) {
		return marshalToolError("internal_path", path+" is cluster-internal and cannot be edited", "")
	}
	oldStr := args["old_string"]
	newStr := args["new_string"]
	if oldStr == "" {
		return marshalToolError("missing_arg", "old_string is required",
			"pass the exact substring to replace")
	}
	if oldStr == newStr {
		return marshalToolError("noop_edit", "old_string equals new_string; nothing to change", "")
	}
	replaceAll := args["replace_all"] == "true"

	lock := lockForPath(path)
	lock.Lock()
	defer lock.Unlock()

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return marshalToolError("path_not_found", path+" does not exist",
				"use write_file to create a new file, or pick an existing path")
		}
		return marshalToolError("read_failed", err.Error(), "")
	}
	bodyStr := string(body)
	count := strings.Count(bodyStr, oldStr)
	if count == 0 {
		return marshalToolError("old_string_not_found",
			fmt.Sprintf("old_string not found in %s", path),
			"read the file first to see its exact contents; whitespace and newlines must match character-for-character")
	}
	if !replaceAll && count > 1 {
		return marshalToolError("ambiguous_match",
			fmt.Sprintf("old_string matches %d times", count),
			"include more surrounding context in old_string to make it unique, or pass replace_all=true to replace every occurrence")
	}

	var updated string
	if replaceAll {
		updated = strings.ReplaceAll(bodyStr, oldStr, newStr)
	} else {
		updated = strings.Replace(bodyStr, oldStr, newStr, 1)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return marshalToolError("write_failed", err.Error(), "")
	}

	out, _ := json.Marshal(map[string]any{
		"path":         path,
		"replacements": count,
		"replace_all":  replaceAll,
	})
	return out, 0, nil
}
