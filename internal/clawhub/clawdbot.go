package clawhub

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ClawdbotMetadata is the structured form of the metadata.clawdbot
// block in a clawhub-format SKILL.md. ClawHub bundles encode their
// runtime requirements + install methods here; lobslaw consumes
// them via SatisfyBinaryRequirements at install time.
type ClawdbotMetadata struct {
	Emoji    string             `yaml:"emoji,omitempty"`
	Requires ClawdbotRequires   `yaml:"requires,omitempty"`
	Install  []ClawdbotInstall  `yaml:"install,omitempty"`
	Setup    []ClawdbotSetup    `yaml:"setup,omitempty"`
}

// ClawdbotRequires lists host-level dependencies a skill needs. Today
// only `bins` is meaningful (binaries that must resolve on PATH);
// future fields will be additive (runtimes, services, env presence).
type ClawdbotRequires struct {
	Bins []string `yaml:"bins,omitempty"`
}

// ClawdbotInstall is one install method declared by the bundle.
// `kind` selects the package manager (brew, apt, pacman, dnf, apk,
// pipx, uvx, npm, cargo, go-install, curl-sh). Other fields are
// kind-specific; unrecognised fields are tolerated for forward-compat.
type ClawdbotInstall struct {
	ID       string   `yaml:"id,omitempty"`
	Kind     string   `yaml:"kind"`
	Label    string   `yaml:"label,omitempty"`
	Bins     []string `yaml:"bins,omitempty"`

	// Manager-specific fields.
	Formula  string   `yaml:"formula,omitempty"`  // brew tap/formula
	Package  string   `yaml:"package,omitempty"`  // apt/pacman/dnf/apk/pipx/uvx/npm/cargo/go-install
	URL      string   `yaml:"url,omitempty"`      // curl-sh
	Checksum string   `yaml:"checksum,omitempty"` // curl-sh: sha256:<hex>
	Sudo     bool     `yaml:"sudo,omitempty"`
	Args     []string `yaml:"args,omitempty"`
	Distro   string   `yaml:"distro,omitempty"`
	OS       string   `yaml:"os,omitempty"`
	Arch     string   `yaml:"arch,omitempty"`
}

// ClawdbotSetup is reserved for future structured one-shot
// post-install commands. ClawHub doesn't define this today —
// kept here as a deliberate hook so the loader doesn't break
// when the field appears.
type ClawdbotSetup struct {
	Cmd  string   `yaml:"cmd,omitempty"`
	Args []string `yaml:"args,omitempty"`
}

// SkillFrontmatter wraps the front-matter block of a clawhub-style
// SKILL.md. Captures the canonical fields plus the metadata
// container.
type SkillFrontmatter struct {
	Name        string                 `yaml:"name"`
	Description string                 `yaml:"description,omitempty"`
	Homepage    string                 `yaml:"homepage,omitempty"`
	Metadata    skillFrontmatterMeta   `yaml:"metadata,omitempty"`
}

type skillFrontmatterMeta struct {
	Clawdbot ClawdbotMetadata `yaml:"clawdbot,omitempty"`
}

// Clawdbot returns the parsed clawdbot block. Convenience accessor
// so callers don't reach into the nested struct.
func (s SkillFrontmatter) Clawdbot() ClawdbotMetadata { return s.Metadata.Clawdbot }

// ParseSkillMD splits a SKILL.md byte slice into front-matter (parsed)
// and prose body. Returns ErrNoFrontmatter when the file doesn't open
// with a YAML front-matter block. The prose return is everything
// after the closing "---" line, with leading newlines trimmed.
func ParseSkillMD(content []byte) (*SkillFrontmatter, string, error) {
	const sep = "---"
	body := bytes.TrimLeft(content, "\r\n ")
	if !bytes.HasPrefix(body, []byte(sep)) {
		return nil, "", ErrNoFrontmatter
	}
	rest := body[len(sep):]
	rest = bytes.TrimLeft(rest, "\r\n ")
	endIdx := bytes.Index(rest, []byte("\n"+sep))
	if endIdx < 0 {
		return nil, "", fmt.Errorf("clawhub: SKILL.md front-matter not terminated by --- line")
	}
	yamlBody := rest[:endIdx]
	prose := rest[endIdx+len("\n"+sep):]
	prose = bytes.TrimLeft(prose, "\r\n ")

	var fm SkillFrontmatter
	if err := yaml.Unmarshal(yamlBody, &fm); err != nil {
		return nil, "", fmt.Errorf("clawhub: parse front-matter: %w", err)
	}
	if strings.TrimSpace(fm.Name) == "" {
		return nil, "", errors.New("clawhub: front-matter missing name")
	}
	return &fm, string(prose), nil
}

// ErrNoFrontmatter is returned when a file doesn't start with a YAML
// front-matter delimiter. Callers use this as a format-detection
// signal — a SKILL.md without front-matter isn't a clawhub bundle.
var ErrNoFrontmatter = errors.New("clawhub: no YAML front-matter")

// ManagerKind translates a clawdbot install.kind to the lobslaw
// internal/binaries Manager name. Returns the empty string for
// unknown kinds — caller decides whether to skip or error.
func ManagerKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "brew":
		return "brew"
	case "apt", "deb", "debian":
		return "apt"
	case "pacman", "arch":
		return "pacman"
	case "dnf", "yum", "fedora", "rhel":
		return "dnf"
	case "apk", "alpine":
		return "apk"
	case "pipx":
		return "pipx"
	case "uvx", "uv-tool":
		return "uvx"
	case "npm":
		return "npm"
	case "cargo":
		return "cargo"
	case "go-install", "go":
		return "go-install"
	case "curl-sh", "curl|sh", "shell-script":
		return "curl-sh"
	case "gh-release", "github-release":
		return "gh-release"
	}
	return ""
}
