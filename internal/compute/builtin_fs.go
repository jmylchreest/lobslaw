package compute

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// readFileLineCap prevents a malicious or overeager tool call from
// siphoning an entire log file into the LLM context. 2000 matches
// opencode's read ceiling — the model almost never needs more.
const (
	readFileDefaultLimit = 200
	readFileMaxLimit     = 2000
	readFileLineCharCap  = 2000

	listFilesDefaultLimit = 200
	listFilesMaxLimit     = 1000
	globMaxMatches        = 500
)

// internalExcludes are directory/glob patterns the fs builtins must
// never expose. These guard cluster-private state (Raft snapshots,
// encrypted bbolt store, TLS keys) even when the operator configures
// a mount that overlaps with the data dir. Matched against the path
// basename OR any path segment.
var internalExcludes = []string{
	".snapshot",
	".git",
	".raft",
	"state.db",
	"state.db.lock",
	"*.key",
	"*.pem",
	"*.jwt",
}

// toolError is the structured failure shape fs/exec builtins emit
// on error. Mirrors opencode's pattern: every failure carries a
// category + human message + actionable next step the LLM can
// follow. Returning this as stdout JSON (exit code 0, but with
// error_type set) keeps the LLM's tool-call result parseable as
// JSON every time — saves it from regex-splitting stderr.
type toolError struct {
	ErrorType  string `json:"error_type"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
}

// marshalToolError encodes the structured error. Returns exitCode=1
// so the executor still treats it as a tool failure, but the JSON
// body carries actionable detail. Use instead of fmt.Errorf in the
// fs builtins.
func marshalToolError(errType, msg, suggestion string) ([]byte, int, error) {
	payload, err := json.Marshal(toolError{
		ErrorType:  errType,
		Message:    msg,
		Suggestion: suggestion,
	})
	if err != nil {
		return nil, 1, fmt.Errorf("%s: %s", errType, msg)
	}
	return payload, 1, nil
}

// isInternalPath returns true when path should be hidden from fs
// builtins (listed, read, or written). Match is on the basename and
// each intermediate segment — any hit wins.
func isInternalPath(path string) bool {
	segments := strings.Split(filepath.ToSlash(path), "/")
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		for _, pat := range internalExcludes {
			if ok, _ := filepath.Match(pat, seg); ok {
				return true
			}
		}
	}
	return false
}

// readFileBuiltin streams a text file with offset/limit paging.
// JSON output so the model can reason about line numbers without
// re-parsing.
func readFileBuiltin(_ context.Context, args map[string]string) ([]byte, int, error) {
	path := strings.TrimSpace(args["path"])
	if path == "" {
		return marshalToolError("missing_arg", "path is required",
			"pass path as an absolute filesystem path string")
	}
	resolved, errPayload, errExit := resolveFsPath(path, false)
	if errExit != 0 {
		return errPayload, errExit, nil
	}
	if resolved != "" {
		path = resolved
	}
	if !filepath.IsAbs(path) {
		return marshalToolError("relative_path", "path must be absolute OR mount-scoped (e.g. 'workspace/notes.md')",
			"prefix with / for absolute, or use a mount label (see debug_storage for known mounts)")
	}
	if isInternalPath(path) {
		return marshalToolError("internal_path", path+" is cluster-internal and cannot be read",
			"this file holds private state (Raft snapshot, TLS key, etc.). Try a different path; workspace mounts are the typical target")
	}
	offset := 0
	if raw, ok := args["offset"]; ok && raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}
	limit := readFileDefaultLimit
	if raw, ok := args["limit"]; ok && raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > readFileMaxLimit {
		limit = readFileMaxLimit
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return marshalToolError("path_not_found", path+" does not exist",
				"call list_files on the parent directory to see what's there, or glob to search by pattern")
		}
		if os.IsPermission(err) {
			return marshalToolError("permission_denied", path+": permission denied",
				"the lobslaw process cannot read this file; try a path inside a configured storage mount")
		}
		return marshalToolError("read_failed", "open "+path+": "+err.Error(),
			"check the path exists and is readable")
	}
	defer f.Close()

	if fi, statErr := f.Stat(); statErr == nil && fi.IsDir() {
		return marshalToolError("is_directory", path+" is a directory, not a file",
			"use list_files to enumerate directory entries, or glob to find files matching a pattern")
	}

	scanner := bufio.NewScanner(f)
	// Bump the max-token size — default 64KB chokes on long log lines.
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)

	var (
		collected []string
		total     int
	)
	for scanner.Scan() {
		total++
		if total-1 < offset {
			continue
		}
		if len(collected) >= limit {
			continue
		}
		line := scanner.Text()
		if len(line) > readFileLineCharCap {
			line = line[:readFileLineCharCap] + "…[truncated]"
		}
		collected = append(collected, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, 1, fmt.Errorf("read_file: scan: %w", err)
	}

	out, err := json.Marshal(map[string]any{
		"path":        path,
		"line_count":  total,
		"offset":      offset,
		"returned":    len(collected),
		"content":     strings.Join(collected, "\n"),
	})
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}

// listFilesBuiltin returns directory entries with name, is_dir, size.
// Absolute path required; internal paths hidden; output capped.
func listFilesBuiltin(_ context.Context, args map[string]string) ([]byte, int, error) {
	path := strings.TrimSpace(args["path"])
	if path == "" {
		return marshalToolError("missing_arg", "path is required",
			"pass path as an absolute directory path string")
	}
	resolved, errPayload, errExit := resolveFsPath(path, false)
	if errExit != 0 {
		return errPayload, errExit, nil
	}
	if resolved != "" {
		path = resolved
	}
	if !filepath.IsAbs(path) {
		return marshalToolError("relative_path", "path must be absolute OR mount-scoped",
			"use '/abs/path' or 'mount-label/subpath' (see debug_storage for mounts)")
	}
	if isInternalPath(path) {
		return marshalToolError("internal_path", path+" is a cluster-internal path",
			"try a user-facing directory like a configured storage mount")
	}
	limit := listFilesDefaultLimit
	if raw, ok := args["limit"]; ok && raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > listFilesMaxLimit {
		limit = listFilesMaxLimit
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return marshalToolError("path_not_found", path+" does not exist",
				"try list_files on a parent directory, or use glob to search by pattern")
		}
		if os.IsPermission(err) {
			return marshalToolError("permission_denied", path+": permission denied", "")
		}
		return marshalToolError("stat_failed", err.Error(), "")
	}
	if !info.IsDir() {
		return marshalToolError("is_file", path+" is a file, not a directory",
			"use read_file to read its contents")
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return marshalToolError("read_failed", err.Error(), "")
	}

	type entry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size,omitempty"`
	}
	out := make([]entry, 0, len(entries))
	truncated := false
	for _, e := range entries {
		if isInternalPath(e.Name()) {
			continue
		}
		if len(out) >= limit {
			truncated = true
			break
		}
		fi, ferr := e.Info()
		var size int64
		if ferr == nil && !e.IsDir() {
			size = fi.Size()
		}
		out = append(out, entry{Name: e.Name(), IsDir: e.IsDir(), Size: size})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})

	payload, err := json.Marshal(map[string]any{
		"path":      path,
		"entries":   out,
		"count":     len(out),
		"truncated": truncated,
	})
	if err != nil {
		return nil, 1, err
	}
	return payload, 0, nil
}

// globBuiltin walks a root directory matching entries against a
// glob pattern. Uses doublestar-style `**` semantics via a simple
// walker so `**/*.md` finds markdown files at any depth. Result set
// is capped at globMaxMatches.
func globBuiltin(ctx context.Context, args map[string]string) ([]byte, int, error) {
	pattern := strings.TrimSpace(args["pattern"])
	if pattern == "" {
		return marshalToolError("missing_arg", "pattern is required",
			"pass a glob pattern like \"**/*.md\" or \"*.go\"")
	}
	root := strings.TrimSpace(args["path"])
	if root == "" {
		return marshalToolError("missing_arg", "path is required",
			"pass an absolute root directory to walk")
	}
	resolved, errPayload, errExit := resolveFsPath(root, false)
	if errExit != 0 {
		return errPayload, errExit, nil
	}
	if resolved != "" {
		root = resolved
	}
	if !filepath.IsAbs(root) {
		return marshalToolError("relative_path", "path must be absolute OR mount-scoped",
			"use '/abs/path' or 'mount-label/subpath'")
	}
	if isInternalPath(root) {
		return marshalToolError("internal_path", root+" is a cluster-internal path", "")
	}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return marshalToolError("path_not_found", root+" does not exist",
				"try list_files on a parent directory first to find a valid root")
		}
		return marshalToolError("stat_failed", err.Error(), "")
	}

	type match struct {
		Path  string `json:"path"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size,omitempty"`
	}
	var matches []match
	truncated := false

	err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isInternalPath(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(matches) >= globMaxMatches {
			truncated = true
			return filepath.SkipAll
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if matchGlob(pattern, rel) || matchGlob(pattern, filepath.Base(p)) {
			var size int64
			if !d.IsDir() {
				if fi, ferr := d.Info(); ferr == nil {
					size = fi.Size()
				}
			}
			matches = append(matches, match{Path: p, IsDir: d.IsDir(), Size: size})
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, 1, fmt.Errorf("glob: %w", err)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })

	payload, err := json.Marshal(map[string]any{
		"pattern":   pattern,
		"root":      root,
		"matches":   matches,
		"count":     len(matches),
		"truncated": truncated,
	})
	if err != nil {
		return nil, 1, err
	}
	return payload, 0, nil
}

// matchGlob supports `**` as a multi-segment wildcard in addition to
// filepath.Match's single-segment semantics. Pattern is converted
// to a regex-equivalent by splitting on `**`, matching each piece
// with filepath.Match, and requiring contiguous coverage.
func matchGlob(pattern, name string) bool {
	name = filepath.ToSlash(name)
	pattern = filepath.ToSlash(pattern)
	if !strings.Contains(pattern, "**") {
		ok, _ := filepath.Match(pattern, name)
		return ok
	}
	parts := strings.Split(pattern, "**")
	idx := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		p := strings.Trim(part, "/")
		// First part must match from start; middle/last may skip ahead.
		if i == 0 {
			segs := strings.SplitN(name, "/", strings.Count(p, "/")+2)
			if len(segs) <= strings.Count(p, "/") {
				return false
			}
			prefix := strings.Join(segs[:strings.Count(p, "/")+1], "/")
			if ok, _ := filepath.Match(p, prefix); !ok {
				return false
			}
			idx = len(prefix) + 1
			continue
		}
		// Search for p in the remaining tail, segment-wise.
		tail := name[min(idx, len(name)):]
		if i == len(parts)-1 && !strings.HasSuffix(pattern, "**") {
			// Last piece must match suffix.
			base := filepath.Base(name)
			if ok, _ := filepath.Match(p, base); ok {
				return true
			}
			// Fallback: try relative match.
			if ok, _ := filepath.Match(p, tail); ok {
				return true
			}
			return false
		}
		if !strings.Contains(tail, p) {
			return false
		}
	}
	return true
}

// searchFilesBuiltin shells out to ripgrep when available, grep -rn
// otherwise. Result set is capped so a pathological pattern doesn't
// flood the model's context.
const searchFilesMaxMatches = 100

func searchFilesBuiltin(ctx context.Context, args map[string]string) ([]byte, int, error) {
	pattern := strings.TrimSpace(args["pattern"])
	if pattern == "" {
		return marshalToolError("missing_arg", "pattern is required",
			"pass a regex pattern to search for")
	}
	searchPath := strings.TrimSpace(args["path"])
	if searchPath == "" {
		return marshalToolError("missing_arg", "path is required",
			"pass an absolute directory or file path to search under. grep does NOT search the web or GitHub — use fetch_url for that")
	}
	resolved, errPayload, errExit := resolveFsPath(searchPath, false)
	if errExit != 0 {
		return errPayload, errExit, nil
	}
	if resolved != "" {
		searchPath = resolved
	}
	if !filepath.IsAbs(searchPath) {
		return marshalToolError("relative_path", "path must be absolute OR mount-scoped",
			"use '/abs/path' or 'mount-label/subpath'. grep is local-filesystem only — for web/GitHub use fetch_url")
	}
	if isInternalPath(searchPath) {
		return marshalToolError("internal_path", searchPath+" is a cluster-internal path", "")
	}
	if _, err := os.Stat(searchPath); err != nil {
		if os.IsNotExist(err) {
			return marshalToolError("path_not_found", searchPath+" does not exist",
				"try list_files on a parent directory to find a valid path. grep searches LOCAL files only — for web/GitHub use fetch_url or web_search")
		}
		return marshalToolError("stat_failed", err.Error(), "")
	}
	glob := strings.TrimSpace(args["glob"])

	bin, argv := chooseSearchCommand(pattern, searchPath, glob)
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin"}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdout, err := cmd.Output()
	// Exit code 1 = no matches (rg + grep agree) — surface as an
	// empty result set, not an error.
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			empty, merr := json.Marshal(map[string]any{
				"pattern": pattern,
				"matches": []any{},
			})
			if merr != nil {
				return nil, 1, merr
			}
			return empty, 0, nil
		}
		stderr := strings.TrimSpace(stderrBuf.String())
		if stderr == "" {
			stderr = err.Error()
		}
		// Exit 2 from rg/grep typically means bad regex or unreadable path.
		return marshalToolError("search_failed", stderr,
			"if the error mentions 'regex', simplify the pattern. If it mentions the path, verify it exists with list_files")
	}

	matches := parseSearchOutput(bin, stdout)
	if len(matches) > searchFilesMaxMatches {
		matches = matches[:searchFilesMaxMatches]
	}
	out, err := json.Marshal(map[string]any{
		"pattern": pattern,
		"path":    searchPath,
		"matches": matches,
	})
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}

// chooseSearchCommand prefers ripgrep when installed — it's faster
// and respects .gitignore by default. Falls back to grep -rn so the
// tool works on bare minimal systems.
func chooseSearchCommand(pattern, path, glob string) (string, []string) {
	if rg, err := exec.LookPath("rg"); err == nil {
		args := []string{"--no-heading", "--line-number", "--max-count=10"}
		if glob != "" {
			args = append(args, "--glob", glob)
		}
		args = append(args, "--", pattern, path)
		return rg, args
	}
	args := []string{"-rn"}
	if glob != "" {
		args = append(args, "--include="+glob)
	}
	args = append(args, "-e", pattern, path)
	return "/usr/bin/grep", args
}

// parseSearchOutput handles both ripgrep and grep line shapes:
// "<path>:<line>:<content>". grep's --include-omitted path may
// start with ./ — tolerate either.
func parseSearchOutput(bin string, raw []byte) []map[string]any {
	_ = bin
	out := []map[string]any{}
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		ln, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		content := parts[2]
		if len(content) > readFileLineCharCap {
			content = content[:readFileLineCharCap] + "…[truncated]"
		}
		out = append(out, map[string]any{
			"path":        parts[0],
			"line_number": ln,
			"line":        content,
		})
	}
	return out
}
