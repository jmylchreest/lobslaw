package clawhub

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jmylchreest/lobslaw/internal/binaries"
)

// ProcessResult captures what came out of processing a clawhub
// bundle: the canonical artefacts on disk + the install specs the
// caller (Installer.Install) should hand to binaries.Satisfier
// before promoting the staging dir into place.
type ProcessResult struct {
	// Name is the skill name from SKILL.md front-matter (or the
	// manifest.yaml's Name field when the bundle is lobslaw-native).
	Name string

	// Format identifies which path the bundle followed: "clawhub"
	// (SKILL.md front-matter) or "native" (manifest.yaml). Mutually
	// exclusive — a bundle with both is treated as native.
	Format string

	// RequiresBins is the host-binary names declared in
	// clawdbot.requires.bins. Satisfier.Satisfy(name, InstallSpecs)
	// is the next step. Empty for native bundles (their
	// requires_binary lives in the manifest itself).
	RequiresBins []string

	// InstallSpecs is the satisfier-shaped translation of
	// clawdbot.install. Empty for native bundles.
	InstallSpecs []binaries.InstallSpec

	// Prose is the SKILL.md prose body (everything after the
	// front-matter). Used by promptgen to inject as the skill's
	// per-turn prompt section. Empty for native bundles.
	Prose string

	// SkippedInstalls collects diagnostics for clawdbot.install
	// entries with unrecognised "kind" values. Useful for surfacing
	// "this bundle declared a snap install method which lobslaw
	// doesn't support" without failing the whole install.
	SkippedInstalls []string
}

// ProcessBundle extracts bundle bytes into stagingDir, detects format,
// and (for clawhub-format bundles) synthesizes a manifest.yaml the
// existing skills watcher will pick up. Caller is responsible for
// promoting stagingDir → installDir via os.Rename and for calling
// Satisfier.Satisfy on the returned RequiresBins+InstallSpecs.
//
// Format precedence: a bundle containing both SKILL.md and
// manifest.yaml is treated as native (manifest.yaml wins). The
// SKILL.md is preserved on disk; promptgen reads it for the
// prose section regardless of format.
func ProcessBundle(bundle []byte, stagingDir string) (*ProcessResult, error) {
	if len(bundle) > MaxBundleSize {
		return nil, fmt.Errorf("clawhub: bundle exceeds %d bytes", MaxBundleSize)
	}
	if err := extractAny(bundle, stagingDir); err != nil {
		return nil, err
	}

	manifestPath := filepath.Join(stagingDir, "manifest.yaml")
	skillMDPath := filepath.Join(stagingDir, "SKILL.md")

	if _, err := os.Stat(manifestPath); err == nil {
		name, err := readManifestName(manifestPath)
		if err != nil {
			return nil, err
		}
		return &ProcessResult{Name: name, Format: "native"}, nil
	}

	skillMD, err := os.ReadFile(skillMDPath)
	if err != nil {
		return nil, fmt.Errorf("clawhub: bundle missing both manifest.yaml and SKILL.md")
	}

	fm, prose, err := ParseSkillMD(skillMD)
	if err != nil {
		return nil, fmt.Errorf("clawhub: parse SKILL.md: %w", err)
	}
	cb := fm.Clawdbot()
	specs, skipped, synthErr := SynthesizeInstallSpecs(cb.Install)
	if synthErr != nil && len(specs) == 0 && len(cb.Requires.Bins) > 0 {
		return nil, fmt.Errorf("clawhub: SKILL.md declares bins but no usable install method: %w", synthErr)
	}

	if err := writeSyntheticManifest(stagingDir, fm, cb); err != nil {
		return nil, err
	}

	return &ProcessResult{
		Name:            fm.Name,
		Format:          "clawhub",
		RequiresBins:    append([]string(nil), cb.Requires.Bins...),
		InstallSpecs:    specs,
		Prose:           prose,
		SkippedInstalls: skipped,
	}, nil
}

// extractAny detects bundle compression — zip vs tar.gz — and
// extracts into dst. Both formats are common in clawhub-style
// catalogs (the actual clawhub.ai serves zip; signed lobslaw-native
// bundles use tar.gz). Detection is by magic bytes only.
func extractAny(bundle []byte, dst string) error {
	if len(bundle) >= 2 && bundle[0] == 0x1f && bundle[1] == 0x8b {
		return extractTarGzBytes(bundle, dst)
	}
	if len(bundle) >= 4 && bundle[0] == 0x50 && bundle[1] == 0x4b && (bundle[2] == 0x03 || bundle[2] == 0x05 || bundle[2] == 0x07) {
		return extractZipBytes(bundle, dst)
	}
	return errors.New("clawhub: bundle is neither tar.gz nor zip")
}

func extractTarGzBytes(bundle []byte, dst string) error {
	return extractTarGz(bytes.NewReader(bundle), dst)
}

func extractZipBytes(bundle []byte, dst string) error {
	rdr, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		return fmt.Errorf("clawhub: zip open: %w", err)
	}
	for _, f := range rdr.File {
		if err := guardEntryPath(f.Name); err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.Clean(f.Name))
		if !strings.HasPrefix(target+string(os.PathSeparator), filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("clawhub: zip entry %q escapes install root", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("clawhub: mkdir %q: %w", f.Name, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("clawhub: mkdir %q: %w", filepath.Dir(f.Name), err)
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
		if err != nil {
			return fmt.Errorf("clawhub: open %q: %w", f.Name, err)
		}
		rc, err := f.Open()
		if err != nil {
			_ = out.Close()
			return fmt.Errorf("clawhub: zip read %q: %w", f.Name, err)
		}
		if _, err := io.Copy(out, io.LimitReader(rc, MaxBundleSize)); err != nil {
			_ = rc.Close()
			_ = out.Close()
			return fmt.Errorf("clawhub: copy %q: %w", f.Name, err)
		}
		_ = rc.Close()
		if err := out.Close(); err != nil {
			return fmt.Errorf("clawhub: close %q: %w", f.Name, err)
		}
	}
	return nil
}

func readManifestName(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("clawhub: read manifest.yaml: %w", err)
	}
	var stub struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(raw, &stub); err != nil {
		return "", fmt.Errorf("clawhub: parse manifest.yaml: %w", err)
	}
	if strings.TrimSpace(stub.Name) == "" {
		return "", errors.New("clawhub: manifest.yaml missing name")
	}
	return stub.Name, nil
}

// writeSyntheticManifest emits a manifest.yaml the existing skills
// watcher will pick up. The synthesised manifest exposes ONE tool
// per skill (the skill name itself), takes a free-form
// command+args shape, and references the binaries on PATH.
//
// SKILL.md prose is preserved as-is on disk; promptgen reads it
// from the install dir at turn time.
func writeSyntheticManifest(stagingDir string, fm *SkillFrontmatter, cb ClawdbotMetadata) error {
	manifest := syntheticManifest{
		SchemaVersion: 1,
		Name:          fm.Name,
		Description:   fm.Description,
		Runtime:       "bash",
		Handler:       "handler.sh",
		RequiresBinary: append([]string(nil), cb.Requires.Bins...),
	}
	if len(cb.Requires.Bins) > 0 {
		manifest.Tools = []syntheticTool{{
			Name:        fm.Name,
			Description: fmt.Sprintf("Run %s. See the SKILL.md prompt section for command syntax.", fm.Name),
			ParametersSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The subcommand to run (e.g. 'gmail search', 'calendar events').",
					},
					"args": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Additional arguments to pass.",
					},
				},
				"required":             []string{"command"},
				"additionalProperties": false,
			},
			RiskTier: "communicating",
			Argv:     []string{cb.Requires.Bins[0], "{{command}}"},
		}}
	}

	out, err := yaml.Marshal(&manifest)
	if err != nil {
		return fmt.Errorf("clawhub: marshal synthetic manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir, "manifest.yaml"), out, 0o644); err != nil {
		return fmt.Errorf("clawhub: write synthetic manifest: %w", err)
	}

	handler := []byte("#!/bin/sh\nexec \"$@\"\n")
	handlerPath := filepath.Join(stagingDir, "handler.sh")
	if err := os.WriteFile(handlerPath, handler, 0o755); err != nil {
		return fmt.Errorf("clawhub: write handler: %w", err)
	}
	return nil
}

type syntheticManifest struct {
	SchemaVersion  int             `yaml:"schema_version"`
	Name           string          `yaml:"name"`
	Description    string          `yaml:"description,omitempty"`
	Runtime        string          `yaml:"runtime"`
	Handler        string          `yaml:"handler"`
	RequiresBinary []string        `yaml:"requires_binary,omitempty"`
	Tools          []syntheticTool `yaml:"tools,omitempty"`
}

type syntheticTool struct {
	Name             string         `yaml:"name"`
	Description      string         `yaml:"description"`
	ParametersSchema map[string]any `yaml:"parameters_schema"`
	RiskTier         string         `yaml:"risk_tier"`
	Argv             []string       `yaml:"argv"`
}
