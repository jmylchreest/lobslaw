package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveExistingPath follows existing components even when a write's
// destination does not exist yet. Resolve before cleaning: link/.. is
// relative to the link's target, not to the directory containing the link.
// This is a point-in-time accident guard, not protection against a process
// swapping symlinks after the check; the sandbox remains that boundary.
func resolveExistingPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		path = cwd + string(filepath.Separator) + path
	}
	volume := filepath.VolumeName(path)
	resolved := volume + string(filepath.Separator)
	parts := strings.Split(filepath.ToSlash(strings.TrimPrefix(path, volume)), "/")
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		candidate := filepath.Join(resolved, part)
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			resolved = candidate
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect path component %q: %w", candidate, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = candidate
			continue
		}
		links++
		if links > maxPathSymlinks {
			return "", fmt.Errorf("resolve path: more than %d symbolic links", maxPathSymlinks)
		}
		target, err := os.Readlink(candidate)
		if err != nil {
			return "", fmt.Errorf("read symbolic link %q: %w", candidate, err)
		}
		if filepath.IsAbs(target) {
			volume = filepath.VolumeName(target)
			resolved = volume + string(filepath.Separator)
			target = strings.TrimPrefix(target, volume)
		}
		parts = append(strings.Split(filepath.ToSlash(target), "/"), parts...)
	}
	return resolved, nil
}
