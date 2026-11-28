package trace

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// The local sink: newline-delimited JSON, per node, on disk.
//
// R24's design said "exported, not stored — no raft bucket and no
// reporting command", and the argument was right about RAFT: a trace
// is high-volume, short-lived, and not agreed-upon state, so
// replicating it would put telemetry into the consensus path.
//
// A per-node file is not raft. It gives `lobslaw trace <turn-id>`
// without any of that, and it means the record survives a collector
// being down — which is when you most want it. The honest cost is that
// a turn served on node A is not queryable from node B; the trace is
// local because the turn was.
//
// Disposable, like the skill cache. Deleting the directory loses
// telemetry and nothing else.

// DefaultMaxBytes bounds one trace file before it rotates.
//
// Bounded because an unbounded telemetry file on a long-running node
// is a disk-full incident waiting for a quiet week. One rotation is
// kept: enough that a rotation mid-investigation does not lose the
// turn being investigated, and few enough that the ceiling is a number
// an operator can reason about (2 x max).
const DefaultMaxBytes = 64 << 20

// FileName is the current trace file inside the trace directory.
const FileName = "turns.ndjson"

// rotatedName is the single kept predecessor.
const rotatedName = "turns.ndjson.1"

// FileSink appends spans as newline-delimited JSON.
type FileSink struct {
	mu       sync.Mutex
	dir      string
	maxBytes int64
	f        *os.File
	size     int64
}

// NewFileSink opens (or creates) the trace file under dir.
//
// The directory is created here rather than lazily, because a sink
// that cannot write should fail at wiring time — when an operator is
// looking at a boot error — rather than silently drop every span.
func NewFileSink(dir string, maxBytes int64) (*FileSink, error) {
	if dir == "" {
		return nil, fmt.Errorf("trace: file sink needs a directory")
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("trace: create %q: %w", dir, err)
	}
	s := &FileSink{dir: filepath.Clean(dir), maxBytes: maxBytes}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileSink) open() error {
	path := filepath.Join(s.dir, FileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("trace: open %q: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("trace: stat %q: %w", path, err)
	}
	s.f, s.size = f, info.Size()
	return nil
}

// Write appends one span.
func (s *FileSink) Write(span Span) error {
	line, err := json.Marshal(span)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return fmt.Errorf("trace: sink is closed")
	}
	// Rotated BEFORE the write that would exceed the bound, not after.
	// Checking afterwards lets one span push the file past the limit,
	// which for a 64 MiB bound is harmless and for a small one
	// configured by somebody testing is the difference between the
	// setting working and appearing not to.
	if s.size+int64(len(line)) > s.maxBytes {
		if err := s.rotate(); err != nil {
			return err
		}
	}
	n, err := s.f.Write(line)
	s.size += int64(n)
	return err
}

// rotate moves the current file aside, keeping exactly one
// predecessor. Caller holds s.mu.
func (s *FileSink) rotate() error {
	if err := s.f.Close(); err != nil {
		return err
	}
	s.f = nil
	cur := filepath.Join(s.dir, FileName)
	// The previous rotation is overwritten rather than accumulating a
	// numbered series. Two files is a ceiling somebody can reason
	// about; an ever-growing series is the same disk-full problem with
	// extra steps.
	if err := os.Rename(cur, filepath.Join(s.dir, rotatedName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return s.open()
}

// Close flushes and releases the file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// ReadTurn returns every span for one turn, oldest first.
//
// Reads the rotated file first so a turn that straddles a rotation
// comes back whole. A trace missing its opening spans because the file
// rolled over mid-turn would be read as a turn that started at its
// third model call.
func ReadTurn(dir, turnID string) ([]Span, error) {
	var out []Span
	for _, name := range []string{rotatedName, FileName} {
		spans, err := readFileSpans(filepath.Join(dir, name), turnID)
		if err != nil {
			return nil, err
		}
		out = append(out, spans...)
	}
	return out, nil
}

// ListTurns returns the distinct turn ids present, newest first.
func ListTurns(dir string, limit int) ([]string, error) {
	seen := map[string]bool{}
	var ids []string
	// Current file first, since newest-first is what a listing wants
	// and the current file holds the newest spans.
	for _, name := range []string{FileName, rotatedName} {
		spans, err := readFileSpans(filepath.Join(dir, name), "")
		if err != nil {
			return nil, err
		}
		for _, span := range slices.Backward(spans) {
			id := span.TurnID
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
			if limit > 0 && len(ids) >= limit {
				return ids, nil
			}
		}
	}
	return ids, nil
}

// readFileSpans reads one file, optionally filtered to a turn.
//
// A missing file is not an error: a node that has never traced has no
// file, and a rotation that has not happened yet has no predecessor.
// Both are the normal state rather than a problem to report.
func readFileSpans(path, turnID string) ([]Span, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path built from the configured trace dir
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("trace: read %q: %w", path, err)
	}
	var out []Span
	for line := range splitLines(raw) {
		if len(line) == 0 {
			continue
		}
		var s Span
		if err := json.Unmarshal(line, &s); err != nil {
			// One unreadable line must not hide the rest. A trace file
			// is append-only from a process that may have been killed
			// mid-write, so a truncated final line is expected rather
			// than exceptional.
			continue
		}
		if turnID != "" && s.TurnID != turnID {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

// splitLines yields each newline-terminated record.
func splitLines(raw []byte) func(func([]byte) bool) {
	return func(yield func([]byte) bool) {
		start := 0
		for i, b := range raw {
			if b != '\n' {
				continue
			}
			if !yield(raw[start:i]) {
				return
			}
			start = i + 1
		}
		if start < len(raw) {
			// A final line with no newline is a process that died
			// mid-write. Yielded anyway; the JSON decode will reject it
			// if it is truncated, and will accept it if the newline was
			// simply the only thing missing.
			yield(raw[start:])
		}
	}
}
