package compute

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
)

// readFileBuiltin streams a text file with offset/limit paging.
// JSON output so the model can reason about line numbers without
// re-parsing.
func readFileBuiltin(_ context.Context, args map[string]string) ([]byte, int, error) {
	path := strings.TrimSpace(args["path"])
	if path == "" {
		return nil, 2, errors.New("read_file: path is required")
	}
	if !filepath.IsAbs(path) {
		return nil, 2, errors.New("read_file: path must be absolute")
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
		return nil, 1, fmt.Errorf("read_file: %w", err)
	}
	defer f.Close()

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

// searchFilesBuiltin shells out to ripgrep when available, grep -rn
// otherwise. Result set is capped so a pathological pattern doesn't
// flood the model's context.
const searchFilesMaxMatches = 100

func searchFilesBuiltin(ctx context.Context, args map[string]string) ([]byte, int, error) {
	pattern := strings.TrimSpace(args["pattern"])
	if pattern == "" {
		return nil, 2, errors.New("search_files: pattern is required")
	}
	searchPath := strings.TrimSpace(args["path"])
	if searchPath == "" {
		searchPath = "."
	}
	glob := strings.TrimSpace(args["glob"])

	bin, argv := chooseSearchCommand(pattern, searchPath, glob)
	cmd := exec.CommandContext(ctx, bin, argv...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin"}
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
		return nil, 1, fmt.Errorf("search_files: %s: %w", bin, err)
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
