package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// LocalSink persists audit entries to `audit.jsonl` with lumberjack
// rotation. The hash chain survives rotation boundaries: when
// lumberjack rotates to audit.jsonl.1, the final hash in the old
// file remains the PrevHash for the first entry in the new file.
// VerifyChain walks every rotated generation in timestamp order so
// a break at the boundary is detectable.
type LocalSink struct {
	path string

	mu     sync.Mutex
	writer *lumberjack.Logger
	closed bool
}

// LocalConfig tunes the local sink. Path is required; the rest
// picks operator-friendly defaults.
type LocalConfig struct {
	Path       string
	MaxSizeMB  int // default 100
	MaxFiles   int // default 10
	MaxAgeDays int // default 0 (unbounded)
}

// NewLocalSink constructs a local JSONL sink. Fails fast on missing
// path; creates the parent directory if it doesn't exist so
// operators don't have to pre-mkdir.
func NewLocalSink(cfg LocalConfig) (*LocalSink, error) {
	if cfg.Path == "" {
		return nil, errors.New("audit.LocalSink: Path required")
	}
	if cfg.MaxSizeMB == 0 {
		cfg.MaxSizeMB = 100
	}
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = 10
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o755); err != nil {
		return nil, fmt.Errorf("audit.LocalSink: mkdir parent: %w", err)
	}
	return &LocalSink{
		path: cfg.Path,
		writer: &lumberjack.Logger{
			Filename:   cfg.Path,
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxFiles,
			MaxAge:     cfg.MaxAgeDays,
			Compress:   false, // leave uncompressed for grep / VerifyChain
		},
	}, nil
}

// Name satisfies AuditSink.
func (s *LocalSink) Name() string { return "local" }

// Append writes one entry as a single JSON line + "\n" terminator.
// Lumberjack handles rotation based on MaxSize under the same
// mutex — we're guaranteed atomic write-or-rotate-then-write.
func (s *LocalSink) Append(_ context.Context, entry types.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSinkClosed
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("audit.LocalSink: marshal: %w", err)
	}
	raw = append(raw, '\n')
	if _, err := s.writer.Write(raw); err != nil {
		return fmt.Errorf("audit.LocalSink: write: %w", err)
	}
	return nil
}

// Query walks every rotated generation + the live file in
// insertion order and filters. No index — scans are linear in the
// log size, which is fine for the realistic "find last 500 entries
// for this actor" query but would want a bbolt / SQLite index if
// the log grows into multi-GB territory. Noted as a follow-up.
func (s *LocalSink) Query(_ context.Context, filter types.AuditFilter) ([]types.AuditEntry, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSinkClosed
	}
	files, err := s.orderedFilesLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}

	var out []types.AuditEntry
	match := filterMatcher(filter)
	for _, f := range files {
		entries, err := readEntries(f)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !match(e) {
				continue
			}
			out = append(out, e)
			if filter.Limit > 0 && len(out) >= filter.Limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// VerifyChain walks every file in insertion order and recomputes
// each entry's hash from PrevHash. Returns the ID of the first
// entry whose computed hash doesn't match what the SUCCESSOR
// claims its PrevHash should be — that's the chain break. Empty
// logs return ok=true with EntriesChecked=0.
func (s *LocalSink) VerifyChain(_ context.Context) (VerifyResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return VerifyResult{}, ErrSinkClosed
	}
	files, err := s.orderedFilesLocked()
	s.mu.Unlock()
	if err != nil {
		return VerifyResult{}, err
	}

	var (
		prevHash string
		count    int64
	)
	for _, f := range files {
		entries, err := readEntries(f)
		if err != nil {
			return VerifyResult{}, err
		}
		for _, e := range entries {
			count++
			if e.PrevHash != prevHash {
				return VerifyResult{
					OK:             false,
					FirstBreakID:   e.ID,
					EntriesChecked: count,
				}, nil
			}
			prevHash = ComputeHash(e)
		}
	}
	return VerifyResult{OK: true, EntriesChecked: count}, nil
}

// Close flushes + closes the lumberjack writer. Subsequent
// Append/Query/VerifyChain return ErrSinkClosed. Idempotent.
func (s *LocalSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.writer.Close()
}

// orderedFilesLocked returns the rotated files + the live file
// sorted oldest-first. Lumberjack's rotation name is
// "<path>-<UTC-timestamp>.<ext>"; lex-sort is chronological because
// the timestamp is the only variable part. Caller must hold s.mu.
func (s *LocalSink) orderedFilesLocked() ([]string, error) {
	dir := filepath.Dir(s.path)
	base := filepath.Base(s.path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rotated []string
	live := ""
	baseNoExt, ext := splitRotationBase(base)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == base {
			live = filepath.Join(dir, name)
			continue
		}
		if matchesRotation(name, baseNoExt, ext) {
			rotated = append(rotated, filepath.Join(dir, name))
		}
	}
	sort.Strings(rotated)
	if live != "" {
		rotated = append(rotated, live)
	}
	return rotated, nil
}

// splitRotationBase breaks "audit.jsonl" into ("audit", ".jsonl")
// so the rotation-name matcher can look for
// "audit-<timestamp>.jsonl". Lumberjack's exact format:
// "<base-no-ext>-<UTC-timestamp>.<ext>"
func splitRotationBase(name string) (string, string) {
	ext := filepath.Ext(name)
	if ext == "" {
		return name, ""
	}
	return name[:len(name)-len(ext)], ext
}

// matchesRotation reports whether name looks like a lumberjack
// rotation artefact for the configured base. We allow any
// timestamp-looking middle segment rather than parsing it — the
// only consumer of this match is the file listing for VerifyChain,
// and false positives (some other file in the log dir) are filtered
// out by readEntries failing to parse non-JSONL content.
func matchesRotation(name, baseNoExt, ext string) bool {
	if len(name) < len(baseNoExt)+len(ext)+2 {
		return false
	}
	if name[:len(baseNoExt)] != baseNoExt {
		return false
	}
	if name[len(baseNoExt)] != '-' {
		return false
	}
	if ext == "" {
		return true
	}
	return name[len(name)-len(ext):] == ext
}

// readEntries parses one JSONL file into a slice of AuditEntry.
// Lines that fail to parse are skipped — we don't want a single
// corrupt line to block the rest of the chain walk, and the chain-
// break report will catch the break anyway.
func readEntries(path string) ([]types.AuditEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("audit: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return decodeJSONL(f)
}

// decodeJSONL is separated from readEntries so tests can feed a
// byte buffer without touching the filesystem.
func decodeJSONL(r io.Reader) ([]types.AuditEntry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var out []types.AuditEntry
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e types.AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

// filterMatcher returns a predicate matching entries against the
// filter. Zero values = match everything for that field.
func filterMatcher(f types.AuditFilter) func(types.AuditEntry) bool {
	return func(e types.AuditEntry) bool {
		if f.ActorScope != "" && e.ActorScope != f.ActorScope {
			return false
		}
		if f.Action != "" && e.Action != f.Action {
			return false
		}
		if f.Target != "" && e.Target != f.Target {
			return false
		}
		if !f.Since.IsZero() && e.Timestamp.Before(f.Since) {
			return false
		}
		if !f.Until.IsZero() && e.Timestamp.After(f.Until) {
			return false
		}
		return true
	}
}

