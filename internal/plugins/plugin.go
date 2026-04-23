// Package plugins provides the on-disk representation + filesystem
// operations for skill plugins: collections of related skills
// shipped as a single directory. Separate from internal/skills
// (which owns runtime behaviour) so the CLI's install/list/enable
// paths don't pull in the YAML parser and subprocess machinery.
package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PluginManifest is the on-disk shape of a plugin's plugin.yaml.
// Lives at the top of the plugin directory alongside a skills/
// subdir containing zero or more skill dirs.
type PluginManifest struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description,omitempty"`
	Author      string   `yaml:"author,omitempty"`
	Homepage    string   `yaml:"homepage,omitempty"`
	Tags        []string `yaml:"tags,omitempty"`
}

// Plugin is the installed-on-disk representation: manifest +
// resolved paths + install-time SHA. The registry (CLI-facing
// "list" path) computes a fresh SHA-of-the-plugin-dir at install;
// detection of on-disk tampering would compare that to a recorded
// baseline, which lands when lobslaw grows an audit surface for
// plugins.
type Plugin struct {
	Manifest    PluginManifest
	Dir         string // absolute path to the plugin directory
	SHA256      string // hex digest of the plugin.yaml at install time
	Enabled     bool   // reflects presence/absence of ".disabled" marker
	InstalledAt time.Time
}

const (
	// ManifestFile is the conventional filename at the root of a
	// plugin directory.
	ManifestFile = "plugin.yaml"

	// DisabledMarker — when present at the plugin's root, the
	// plugin is considered disabled. Simpler than a separate
	// enabled-plugins registry file and lets operators flip state
	// with `touch` / `rm` for debugging.
	DisabledMarker = ".disabled"

	// SkillsSubdir is the directory under each plugin that holds
	// its skill manifests. Mirrors the Claude-Code layout.
	SkillsSubdir = "skills"
)

// Install copies a source directory into dstRoot/<plugin-name>.
// Reads source/plugin.yaml to determine the plugin name; rejects
// if an install already exists at the destination (use `lobslaw
// plugin update`-style semantics in a future version — a safe
// default today is to refuse rather than stomp).
func Install(source, dstRoot string) (*Plugin, error) {
	source = filepath.Clean(source)
	if !filepath.IsAbs(source) {
		return nil, fmt.Errorf("plugins: source %q must be absolute", source)
	}
	if !filepath.IsAbs(dstRoot) {
		return nil, fmt.Errorf("plugins: destination root %q must be absolute", dstRoot)
	}

	manifest, raw, err := readManifest(filepath.Join(source, ManifestFile))
	if err != nil {
		return nil, err
	}

	dst := filepath.Join(dstRoot, manifest.Name)
	if _, err := os.Stat(dst); err == nil {
		return nil, fmt.Errorf("plugins: plugin %q already installed at %q — remove it first", manifest.Name, dst)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("plugins: stat dst: %w", err)
	}

	if err := copyTree(source, dst); err != nil {
		// Best-effort cleanup; ignore secondary errors.
		_ = os.RemoveAll(dst)
		return nil, fmt.Errorf("plugins: copy: %w", err)
	}

	sum := sha256.Sum256(raw)
	return &Plugin{
		Manifest:    manifest,
		Dir:         dst,
		SHA256:      hex.EncodeToString(sum[:]),
		Enabled:     true,
		InstalledAt: time.Now(),
	}, nil
}

// Uninstall removes a plugin by name. Returns an error for unknown
// plugins so scripted tooling gets a clear failure rather than a
// silent no-op.
func Uninstall(dstRoot, name string) error {
	dir := filepath.Join(dstRoot, name)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("plugins: %q not installed", name)
		}
		return err
	}
	return os.RemoveAll(dir)
}

// List walks dstRoot and returns every installed plugin. Directories
// without a plugin.yaml are silently skipped — not every subdir is a
// plugin (e.g. an operator's ad-hoc "temp/" workspace). Returns
// sorted by name for stable CLI output.
func List(dstRoot string) ([]*Plugin, error) {
	entries, err := os.ReadDir(dstRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("plugins: list %q: %w", dstRoot, err)
	}
	var out []*Plugin
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(dstRoot, e.Name())
		manifestPath := filepath.Join(dir, ManifestFile)
		manifest, raw, err := readManifest(manifestPath)
		if err != nil {
			if _, statErr := os.Stat(manifestPath); os.IsNotExist(statErr) {
				continue
			}
			return nil, err
		}
		sum := sha256.Sum256(raw)
		info, _ := os.Stat(manifestPath)
		var installedAt time.Time
		if info != nil {
			installedAt = info.ModTime()
		}
		_, disabled := os.Stat(filepath.Join(dir, DisabledMarker))
		out = append(out, &Plugin{
			Manifest:    manifest,
			Dir:         dir,
			SHA256:      hex.EncodeToString(sum[:]),
			Enabled:     disabled != nil,
			InstalledAt: installedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.Name < out[j].Manifest.Name })
	return out, nil
}

// Enable removes the .disabled marker if present. Idempotent.
func Enable(dstRoot, name string) error {
	marker := filepath.Join(dstRoot, name, DisabledMarker)
	if err := os.Remove(marker); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("plugins: enable: %w", err)
	}
	return nil
}

// Disable creates the .disabled marker. Idempotent. Returns an
// error if the plugin isn't installed — keeps scripted
// orchestration honest about its state.
func Disable(dstRoot, name string) error {
	dir := filepath.Join(dstRoot, name)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("plugins: %q not installed", name)
		}
		return err
	}
	marker := filepath.Join(dir, DisabledMarker)
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("plugins: disable: %w", err)
	}
	return f.Close()
}

// SkillDirs returns each skill directory under the plugin's
// skills/ subdir. Used by the registry loader to enumerate which
// skill manifests a plugin contributes.
func (p *Plugin) SkillDirs() ([]string, error) {
	root := filepath.Join(p.Dir, SkillsSubdir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(root, e.Name()))
	}
	sort.Strings(out)
	return out, nil
}

// readManifest loads + validates the plugin.yaml.
func readManifest(path string) (PluginManifest, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return PluginManifest{}, nil, fmt.Errorf("plugins: read %q: %w", path, err)
	}
	var m PluginManifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return PluginManifest{}, nil, fmt.Errorf("plugins: parse %q: %w", path, err)
	}
	if m.Name == "" {
		return PluginManifest{}, nil, fmt.Errorf("plugins: %q: name is required", path)
	}
	if strings.ContainsAny(m.Name, "/\\") {
		return PluginManifest{}, nil, fmt.Errorf("plugins: %q: name must not contain path separators", path)
	}
	if m.Version == "" {
		return PluginManifest{}, nil, fmt.Errorf("plugins: %q: version is required", path)
	}
	return m, raw, nil
}

// copyTree recursively copies src into dst, preserving file mode
// bits. Symlinks are followed rather than recreated so the
// installed plugin doesn't retain pointers to the source
// filesystem layout.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}

		if info.Mode()&os.ModeSymlink != 0 {
			// Resolve and copy the target instead of recreating the
			// link — installed plugin should be self-contained.
			resolved, rerr := filepath.EvalSymlinks(path)
			if rerr != nil {
				return rerr
			}
			info, err = os.Stat(resolved)
			if err != nil {
				return err
			}
			if info.IsDir() {
				return os.MkdirAll(target, info.Mode().Perm())
			}
			return copyFile(resolved, target, info.Mode().Perm())
		}

		if !info.Mode().IsRegular() {
			// Skip devices, sockets, named pipes — anything
			// non-regular is operator error in a plugin source dir.
			return nil
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return nil
}

// errNotImpl is a placeholder for `plugin install <ref>` variants
// we haven't wired yet — git URLs, clawhub refs. Present so the
// CLI surface can reject those inputs with a clear message rather
// than mysteriously filesystem-erroring.
var errNotImpl = errors.New("plugins: only local-path sources are supported in this version")

// IsURLLikeSource reports whether src looks like a scheme we
// haven't implemented (git://, https://, clawhub:). Callers surface
// a clear "not yet supported" message.
func IsURLLikeSource(src string) bool {
	for _, prefix := range []string{"git://", "git+", "https://", "http://", "clawhub:", "github:"} {
		if strings.HasPrefix(src, prefix) {
			return true
		}
	}
	return false
}

// ErrUnsupportedSource surfaces unresolvable refs cleanly.
var ErrUnsupportedSource = errNotImpl
