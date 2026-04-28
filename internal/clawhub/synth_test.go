package clawhub

import (
	"strings"
	"testing"
)

func TestSynthesizeInstallSpecsBrewWithFormula(t *testing.T) {
	specs, skipped, err := SynthesizeInstallSpecs([]ClawdbotInstall{{
		Kind:    "brew",
		Formula: "steipete/tap/gogcli",
		Bins:    []string{"gog"},
	}})
	if err != nil {
		t.Fatalf("synth: %v (skipped: %v)", err, skipped)
	}
	if len(specs) != 1 {
		t.Fatalf("specs: %v", specs)
	}
	if specs[0].Manager != "brew" || specs[0].Package != "steipete/tap/gogcli" {
		t.Errorf("spec: %+v", specs[0])
	}
}

func TestSynthesizeInstallSpecsAptDefaultsOSLinux(t *testing.T) {
	specs, _, err := SynthesizeInstallSpecs([]ClawdbotInstall{{
		Kind:    "apt",
		Package: "gh",
		Sudo:    true,
	}})
	if err != nil {
		t.Fatalf("synth: %v", err)
	}
	if specs[0].OS != "linux" {
		t.Errorf("expected OS=linux defaulted, got %q", specs[0].OS)
	}
	if !specs[0].Sudo {
		t.Errorf("sudo not propagated")
	}
}

func TestSynthesizeInstallSpecsCurlSh(t *testing.T) {
	specs, _, err := SynthesizeInstallSpecs([]ClawdbotInstall{{
		Kind:     "curl-sh",
		URL:      "https://astral.sh/uv/install.sh",
		Checksum: "sha256:" + strings.Repeat("0", 64),
	}})
	if err != nil {
		t.Fatalf("synth: %v", err)
	}
	if specs[0].URL != "https://astral.sh/uv/install.sh" {
		t.Errorf("URL: %q", specs[0].URL)
	}
	if !strings.HasPrefix(specs[0].Checksum, "sha256:") {
		t.Errorf("checksum: %q", specs[0].Checksum)
	}
}

func TestSynthesizeInstallSpecsUnknownKindSkipped(t *testing.T) {
	specs, skipped, err := SynthesizeInstallSpecs([]ClawdbotInstall{
		{Kind: "snap", Package: "gh"},
		{Kind: "brew", Formula: "gh"},
	})
	if err != nil {
		t.Fatalf("synth (one valid spec, snap should skip silently): %v", err)
	}
	if len(specs) != 1 || specs[0].Manager != "brew" {
		t.Errorf("specs: %v", specs)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "snap") {
		t.Errorf("skipped: %v", skipped)
	}
}

func TestSynthesizeInstallSpecsAllSkippedErrors(t *testing.T) {
	_, skipped, err := SynthesizeInstallSpecs([]ClawdbotInstall{
		{Kind: "snap", Package: "gh"},
		{Kind: "flatpak", Package: "gh"},
	})
	if err == nil {
		t.Fatal("expected error when no specs survive")
	}
	if len(skipped) != 2 {
		t.Errorf("skipped: %v", skipped)
	}
}

func TestSynthesizeInstallSpecsMultipleManagersOK(t *testing.T) {
	specs, _, err := SynthesizeInstallSpecs([]ClawdbotInstall{
		{Kind: "brew", Formula: "steipete/tap/gogcli"},
		{Kind: "apt", Package: "gogcli", Sudo: true},
		{Kind: "pacman", Package: "gogcli", Sudo: true},
	})
	if err != nil {
		t.Fatalf("synth: %v", err)
	}
	if len(specs) != 3 {
		t.Fatalf("specs: %v", specs)
	}
	managers := []string{specs[0].Manager, specs[1].Manager, specs[2].Manager}
	want := []string{"brew", "apt", "pacman"}
	for i := range want {
		if managers[i] != want[i] {
			t.Errorf("[%d] manager: got %q want %q", i, managers[i], want[i])
		}
	}
}
