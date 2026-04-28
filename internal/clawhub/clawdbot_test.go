package clawhub

import (
	"errors"
	"strings"
	"testing"
)

func TestParseSkillMDValid(t *testing.T) {
	content := []byte(`---
name: gog
description: Google Workspace CLI
homepage: https://gogcli.sh
metadata:
  clawdbot:
    emoji: "🎮"
    requires:
      bins: [gog]
    install:
      - id: brew
        kind: brew
        formula: steipete/tap/gogcli
        bins: [gog]
        label: Install gog (brew)
---

# gog

Use gog for Gmail/Calendar.
`)
	fm, prose, err := ParseSkillMD(content)
	if err != nil {
		t.Fatalf("ParseSkillMD: %v", err)
	}
	if fm.Name != "gog" {
		t.Errorf("name: %q", fm.Name)
	}
	if fm.Homepage != "https://gogcli.sh" {
		t.Errorf("homepage: %q", fm.Homepage)
	}
	cb := fm.Clawdbot()
	if got := cb.Requires.Bins; len(got) != 1 || got[0] != "gog" {
		t.Errorf("requires.bins: %v", got)
	}
	if len(cb.Install) != 1 {
		t.Fatalf("install: %v", cb.Install)
	}
	if cb.Install[0].Kind != "brew" || cb.Install[0].Formula != "steipete/tap/gogcli" {
		t.Errorf("install[0]: %+v", cb.Install[0])
	}
	if !strings.Contains(prose, "# gog") {
		t.Errorf("prose missing heading: %q", prose)
	}
}

func TestParseSkillMDNoFrontmatter(t *testing.T) {
	content := []byte("# Just a markdown file\n\nNo front-matter here.\n")
	_, _, err := ParseSkillMD(content)
	if !errors.Is(err, ErrNoFrontmatter) {
		t.Fatalf("expected ErrNoFrontmatter, got %v", err)
	}
}

func TestParseSkillMDUnterminated(t *testing.T) {
	content := []byte("---\nname: x\n# Forgot the closing ---\nbody body body")
	_, _, err := ParseSkillMD(content)
	if err == nil {
		t.Fatal("expected unterminated error")
	}
	if errors.Is(err, ErrNoFrontmatter) {
		t.Errorf("should not be ErrNoFrontmatter")
	}
}

func TestParseSkillMDMissingName(t *testing.T) {
	content := []byte("---\ndescription: nameless\n---\nbody")
	_, _, err := ParseSkillMD(content)
	if err == nil {
		t.Fatal("expected missing-name error")
	}
}

func TestParseSkillMDInlineJSON(t *testing.T) {
	// Real clawhub bundles emit metadata as a single inline JSON-shaped
	// YAML scalar. Confirm the parser handles that shape.
	content := []byte(`---
name: gog
description: Google Workspace CLI
metadata: {"clawdbot":{"emoji":"🎮","requires":{"bins":["gog"]},"install":[{"id":"brew","kind":"brew","formula":"steipete/tap/gogcli","bins":["gog"],"label":"Install gog (brew)"}]}}
---

# gog
`)
	fm, _, err := ParseSkillMD(content)
	if err != nil {
		t.Fatalf("ParseSkillMD: %v", err)
	}
	cb := fm.Clawdbot()
	if len(cb.Install) != 1 {
		t.Fatalf("install: %v", cb.Install)
	}
	if cb.Install[0].Formula != "steipete/tap/gogcli" {
		t.Errorf("install[0] formula: %q", cb.Install[0].Formula)
	}
}

func TestManagerKind(t *testing.T) {
	cases := map[string]string{
		"brew":         "brew",
		"BREW":         "brew",
		"  brew  ":     "brew",
		"apt":          "apt",
		"deb":          "apt",
		"debian":       "apt",
		"pacman":       "pacman",
		"arch":         "pacman",
		"dnf":          "dnf",
		"yum":          "dnf",
		"apk":          "apk",
		"alpine":       "apk",
		"pipx":         "pipx",
		"uvx":          "uvx",
		"uv-tool":      "uvx",
		"npm":          "npm",
		"cargo":        "cargo",
		"go":           "go-install",
		"go-install":   "go-install",
		"curl-sh":      "curl-sh",
		"shell-script": "curl-sh",
		"unknown":      "",
		"":             "",
	}
	for in, want := range cases {
		got := ManagerKind(in)
		if got != want {
			t.Errorf("ManagerKind(%q) = %q want %q", in, got, want)
		}
	}
}
