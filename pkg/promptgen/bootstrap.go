package promptgen

import (
	"fmt"
	"os"
	"strings"
)

// BootstrapConfig tunes bootstrap-file loading. Per-file and total
// caps protect the prompt from runaway-large files silently bloating
// context — an operator who drops a 10 MB log into the bootstrap
// path should get a clean truncation + warning, not an OOM or a
// $50 LLM call.
type BootstrapConfig struct {
	// Paths is the ordered list of files (or globs — caller pre-
	// expanded) to load and append. Order matters: first file lands
	// at the top of the bootstrap block.
	Paths []string

	// MaxCharsPerFile caps each file individually. 0 = no per-file
	// cap. Truncation appends a `\n… [truncated]\n` sentinel so the
	// model sees that content was cut.
	MaxCharsPerFile int

	// MaxTotalChars caps the combined bootstrap block. Files are
	// loaded in order; once a file's inclusion would push over the
	// total, the file is either truncated to fit or skipped entirely
	// if there's no room left. 0 = no total cap.
	MaxTotalChars int

	// FS is the filesystem abstraction. Defaults to the OS FS.
	// Injectable for tests — SafeReadFile accepts an fs.FS so
	// in-memory filesystems work.
	FS FS
}

// FS is the minimal filesystem interface bootstrap loading needs.
// Satisfied by osFS below and by io/fs implementations that also
// expose Open — we use ReadFile directly so the caller's FS needs
// that single method.
type FS interface {
	ReadFile(path string) ([]byte, error)
}

// osFS is the default FS. Files are resolved via os.ReadFile.
type osFS struct{}

func (osFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// DefaultFS is the package-level FS, swapped out in tests. Public
// so docs/tests can reach it without exporting construction helpers.
var DefaultFS FS = osFS{}

// BootstrapResult carries the assembled block + diagnostics.
// Callers log the diagnostics so operators can see which files
// were truncated / skipped without having to inspect output.
type BootstrapResult struct {
	// Body is the assembled text ready to include in the system
	// prompt (may be empty if no files loaded or everything got
	// skipped).
	Body string

	// Loaded is the list of (path, finalBytes) actually included.
	// Populated in load order.
	Loaded []BootstrapLoaded

	// Truncated is the subset of Loaded that had content cut.
	Truncated []string

	// Skipped lists paths that didn't fit (total cap reached) or
	// failed to read. Skipped != truncated: skipped = "file didn't
	// load at all"; truncated = "some content made it, some didn't".
	Skipped []BootstrapSkipped
}

// BootstrapLoaded records one file that contributed to the result.
type BootstrapLoaded struct {
	Path  string
	Bytes int
}

// BootstrapSkipped records one file that didn't contribute. Reason
// is free-form ("total cap reached", "read error: ...").
type BootstrapSkipped struct {
	Path   string
	Reason string
}

// LoadBootstrap reads the configured files, applies per-file and
// total caps, and assembles a single bootstrap block. The block
// format is one file per subsection with a heading showing the
// source path and (if any) a truncation note.
//
// Read errors are captured in Skipped and NOT returned — one bad
// file shouldn't block the rest of the bootstrap. Callers who
// want fail-fast semantics check len(result.Skipped) themselves.
func LoadBootstrap(cfg BootstrapConfig) BootstrapResult {
	fsys := cfg.FS
	if fsys == nil {
		fsys = DefaultFS
	}

	var result BootstrapResult
	var body strings.Builder
	remaining := cfg.MaxTotalChars // 0 means unlimited; we short-circuit below

	for _, path := range cfg.Paths {
		if cfg.MaxTotalChars > 0 && remaining <= 0 {
			result.Skipped = append(result.Skipped, BootstrapSkipped{
				Path: path, Reason: "total cap reached",
			})
			continue
		}

		raw, err := fsys.ReadFile(path)
		if err != nil {
			result.Skipped = append(result.Skipped, BootstrapSkipped{
				Path: path, Reason: fmt.Sprintf("read: %v", err),
			})
			continue
		}
		content := string(raw)

		// Per-file cap first — keeps a single huge file from
		// starving the rest of the list of budget.
		truncatedPerFile := false
		if cfg.MaxCharsPerFile > 0 && len(content) > cfg.MaxCharsPerFile {
			content = content[:cfg.MaxCharsPerFile] + "\n… [truncated per-file]\n"
			truncatedPerFile = true
		}

		// Total-cap enforcement — if adding the whole file would
		// overshoot, trim to what fits. If even the heading+marker
		// wouldn't fit, skip the file entirely.
		prefix := fmt.Sprintf("<!-- bootstrap: %s -->\n", path)
		truncatedByTotal := false
		if cfg.MaxTotalChars > 0 {
			budget := remaining - len(prefix)
			if budget <= 0 {
				result.Skipped = append(result.Skipped, BootstrapSkipped{
					Path: path, Reason: "total cap reached (no room for heading)",
				})
				continue
			}
			if len(content) > budget {
				suffix := "\n… [truncated to fit total budget]\n"
				if budget <= len(suffix) {
					result.Skipped = append(result.Skipped, BootstrapSkipped{
						Path: path, Reason: "total cap reached (content + suffix exceed)",
					})
					continue
				}
				content = content[:budget-len(suffix)] + suffix
				truncatedByTotal = true
			}
		}

		body.WriteString(prefix)
		body.WriteString(content)
		if !strings.HasSuffix(content, "\n") {
			body.WriteByte('\n')
		}
		body.WriteByte('\n')

		usedBytes := len(prefix) + len(content)
		if cfg.MaxTotalChars > 0 {
			remaining -= usedBytes
		}

		result.Loaded = append(result.Loaded, BootstrapLoaded{Path: path, Bytes: usedBytes})
		if truncatedPerFile || truncatedByTotal {
			result.Truncated = append(result.Truncated, path)
		}
	}

	result.Body = body.String()
	return result
}

// ---------------------------------------------------------------
// Test-only FS helpers — kept in this file (not _test.go) so
// integration tests in other packages can construct an in-memory
// bootstrap FS without importing internal package state.
// ---------------------------------------------------------------

// MapFS is a trivial in-memory FS for tests. Keys are paths, values
// are raw file bytes. Missing keys return os.ErrNotExist.
type MapFS map[string]string

// ReadFile satisfies the FS interface.
func (m MapFS) ReadFile(path string) ([]byte, error) {
	if s, ok := m[path]; ok {
		return []byte(s), nil
	}
	return nil, os.ErrNotExist
}

// The Generate() assembler that composes every section + bootstrap
// into one system prompt lives with the agent loop (Phase 5.4) —
// it depends on types.SoulConfig + time.Time + every section
// builder, so it's the natural caller, not another leaf.
