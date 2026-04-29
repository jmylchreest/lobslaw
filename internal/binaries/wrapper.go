package binaries

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type WrapperStatus int

const (
	WrapperUnchanged WrapperStatus = iota
	WrapperApplied
	WrapperRemoved
	WrapperNotApplicable
)

func (s WrapperStatus) String() string {
	switch s {
	case WrapperApplied:
		return "applied"
	case WrapperRemoved:
		return "removed"
	case WrapperNotApplicable:
		return "not-applicable"
	default:
		return "unchanged"
	}
}

const envWrapperHeader = "#!/bin/sh\n# lobslaw-managed env wrapper — do not edit by hand\n"

// EnsureEnvWrapper installs, refreshes, or removes a /bin/sh shim
// that exports the operator's env vars before exec'ing the real
// binary. Layout:
//
//	<prefix>/bin/<name>      — public command (the shim)
//	<prefix>/libexec/<name>  — the real binary
//
// Returns WrapperNotApplicable when the binary isn't at
// <prefix>/bin/<name> (e.g. apt/brew installed it to a system path).
func EnsureEnvWrapper(prefix, name string, env []string) (WrapperStatus, error) {
	if prefix == "" || name == "" {
		return WrapperUnchanged, fmt.Errorf("binaries: EnsureEnvWrapper requires prefix + name")
	}
	binPath := filepath.Join(prefix, "bin", name)
	libexecPath := filepath.Join(prefix, "libexec", name)

	binFI, binErr := os.Lstat(binPath)
	_, libexecErr := os.Lstat(libexecPath)

	want := renderShim(name, env)
	tearDown := len(env) == 0

	if tearDown {
		if binErr == nil && isOurShim(binPath) {
			if libexecErr == nil {
				if err := os.Remove(binPath); err != nil {
					return WrapperUnchanged, fmt.Errorf("binaries: remove shim: %w", err)
				}
				if err := os.Rename(libexecPath, binPath); err != nil {
					return WrapperUnchanged, fmt.Errorf("binaries: restore real binary: %w", err)
				}
				return WrapperRemoved, nil
			}
			return WrapperUnchanged, nil
		}
		return WrapperUnchanged, nil
	}

	switch {
	case binErr == nil && isOurShim(binPath):
		current, err := os.ReadFile(binPath)
		if err == nil && string(current) == want {
			return WrapperUnchanged, nil
		}
		if err := writeShim(binPath, want); err != nil {
			return WrapperUnchanged, err
		}
		return WrapperApplied, nil

	case binErr == nil && binFI.Mode().IsRegular():
		if libexecErr == nil {
			if err := os.Remove(libexecPath); err != nil {
				return WrapperUnchanged, fmt.Errorf("binaries: clear stale libexec: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(libexecPath), 0o755); err != nil {
			return WrapperUnchanged, fmt.Errorf("binaries: mkdir libexec: %w", err)
		}
		if err := os.Rename(binPath, libexecPath); err != nil {
			return WrapperUnchanged, fmt.Errorf("binaries: move binary to libexec: %w", err)
		}
		if err := writeShim(binPath, want); err != nil {
			_ = os.Rename(libexecPath, binPath)
			return WrapperUnchanged, err
		}
		return WrapperApplied, nil

	case binErr != nil && libexecErr == nil:
		if err := writeShim(binPath, want); err != nil {
			return WrapperUnchanged, err
		}
		return WrapperApplied, nil

	default:
		return WrapperNotApplicable, nil
	}
}

func renderShim(name string, env []string) string {
	pairs := make([]string, 0, len(env))
	for _, kv := range env {
		kv = strings.TrimSpace(kv)
		if kv == "" || !strings.Contains(kv, "=") {
			continue
		}
		pairs = append(pairs, kv)
	}
	sort.Strings(pairs)

	var b strings.Builder
	b.WriteString(envWrapperHeader)
	for _, kv := range pairs {
		eq := strings.IndexByte(kv, '=')
		key := kv[:eq]
		val := kv[eq+1:]
		// Single-quote with the '\'' escape trick so paths/URLs/etc.
		// pass through verbatim regardless of operator content.
		fmt.Fprintf(&b, "export %s='%s'\n", key, strings.ReplaceAll(val, "'", `'\''`))
	}
	fmt.Fprintf(&b, "exec \"$(dirname \"$0\")/../libexec/%s\" \"$@\"\n", name)
	return b.String()
}

func isOurShim(path string) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.HasPrefix(string(body), envWrapperHeader)
}

func writeShim(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("binaries: mkdir bin: %w", err)
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, []byte(body), 0o755); err != nil {
		return fmt.Errorf("binaries: write shim: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("binaries: install shim: %w", err)
	}
	return nil
}
