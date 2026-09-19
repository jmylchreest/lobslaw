package sharing

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type Receipt struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

type Publisher interface {
	Publish(context.Context, Artifact, string) (Receipt, error)
}
type Source interface {
	Fetch(context.Context, string) (Artifact, error)
}

// FileBackend implements both directions. Remote transports implement these
// same narrow interfaces; retrieval never grants installation authority.
type FileBackend struct{}

func filePath(ref string) (string, error) {
	p, ok := strings.CutPrefix(ref, "file:")
	if !ok || p == "" || strings.HasPrefix(p, "//") {
		return "", errors.New("sharing: expected file:<path> (remote backends are not installed)")
	}
	return filepath.Clean(p), nil
}

func (FileBackend) Fetch(ctx context.Context, ref string) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	p, err := filePath(ref)
	if err != nil {
		return Artifact{}, err
	}
	f, err := os.Open(p)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Artifact{}, err
	}
	if !info.Mode().IsRegular() {
		return Artifact{}, errors.New("sharing: source must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxBytes+1))
	if err != nil {
		return Artifact{}, err
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	return Decode(raw)
}

func (FileBackend) Publish(ctx context.Context, a Artifact, ref string) (Receipt, error) {
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	if _, err := Decode(a.raw); err != nil {
		return Receipt{}, err
	}
	p, err := filePath(ref)
	if err != nil {
		return Receipt{}, err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".lobskill-*")
	if err != nil {
		return Receipt{}, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err = f.Write(a.raw); err != nil {
		_ = f.Close()
		return Receipt{}, err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return Receipt{}, err
	}
	if err = f.Close(); err != nil {
		return Receipt{}, err
	}
	if err = ctx.Err(); err != nil {
		return Receipt{}, err
	}
	// Link publishes atomically without clobbering a concurrent destination.
	if err = os.Link(tmp, p); err != nil {
		return Receipt{}, err
	}
	dir, err := os.Open(filepath.Dir(p))
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = dir.Close() }()
	if err = dir.Sync(); err != nil {
		return Receipt{}, err
	}
	return Receipt{Reference: ref, Digest: a.digest}, nil
}
