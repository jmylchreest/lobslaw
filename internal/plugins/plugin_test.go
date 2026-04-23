package plugins

import (
	"os"
	"path/filepath"
	"testing"
)

func writePluginSource(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInstallRequiresAbsolutePaths(t *testing.T) {
	t.Parallel()
	if _, err := Install("./relative", t.TempDir()); err == nil {
		t.Error("relative source should fail")
	}
	if _, err := Install(t.TempDir(), "./dst"); err == nil {
		t.Error("relative dst should fail")
	}
}

func TestInstallHappyPath(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writePluginSource(t, src, `
name: greeter-pack
version: 1.0.0
description: example
`)
	// Add a skill directory under skills/.
	skillDir := filepath.Join(src, SkillsSubdir, "hello")
	_ = os.MkdirAll(skillDir, 0o755)
	_ = os.WriteFile(filepath.Join(skillDir, "manifest.yaml"), []byte("name: hello\nversion: 1.0.0\nruntime: bash\nhandler: h.sh"), 0o644)
	_ = os.WriteFile(filepath.Join(skillDir, "h.sh"), []byte("echo hi"), 0o755)

	dstRoot := t.TempDir()
	p, err := Install(src, dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Name != "greeter-pack" {
		t.Errorf("name: %q", p.Manifest.Name)
	}
	if p.Dir != filepath.Join(dstRoot, "greeter-pack") {
		t.Errorf("dir: %q", p.Dir)
	}

	// Skill was copied too.
	if _, err := os.Stat(filepath.Join(p.Dir, "skills", "hello", "manifest.yaml")); err != nil {
		t.Errorf("skill manifest not copied: %v", err)
	}
}

func TestInstallRefusesDoubleInstall(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writePluginSource(t, src, `
name: double
version: 1.0.0
`)
	dstRoot := t.TempDir()
	if _, err := Install(src, dstRoot); err != nil {
		t.Fatal(err)
	}
	_, err := Install(src, dstRoot)
	if err == nil {
		t.Error("second install of same name should fail")
	}
}

func TestInstallRejectsManifestWithoutName(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writePluginSource(t, src, "version: 1.0.0\n")
	if _, err := Install(src, t.TempDir()); err == nil {
		t.Error("missing name should fail")
	}
}

func TestInstallRejectsNameWithSeparator(t *testing.T) {
	t.Parallel()
	src := t.TempDir()
	writePluginSource(t, src, `
name: foo/bar
version: 1.0.0
`)
	if _, err := Install(src, t.TempDir()); err == nil {
		t.Error("name with / should fail")
	}
}

func TestListHappyPath(t *testing.T) {
	t.Parallel()
	dstRoot := t.TempDir()
	for _, name := range []string{"alpha", "bravo", "charlie"} {
		src := t.TempDir()
		writePluginSource(t, src, "name: "+name+"\nversion: 1.0.0\n")
		if _, err := Install(src, dstRoot); err != nil {
			t.Fatal(err)
		}
	}
	list, err := List(dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("len=%d", len(list))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if list[i].Manifest.Name != w {
			t.Errorf("[%d] got %q want %q", i, list[i].Manifest.Name, w)
		}
	}
}

func TestListSkipsNonPluginDirs(t *testing.T) {
	t.Parallel()
	dstRoot := t.TempDir()
	// One valid plugin.
	src := t.TempDir()
	writePluginSource(t, src, "name: real\nversion: 1.0.0\n")
	_, _ = Install(src, dstRoot)
	// Another directory without a plugin.yaml.
	junk := filepath.Join(dstRoot, "operator-notes")
	_ = os.MkdirAll(junk, 0o755)
	_ = os.WriteFile(filepath.Join(junk, "README.md"), []byte("# nope"), 0o644)

	list, err := List(dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Manifest.Name != "real" {
		t.Errorf("should ignore manifestless dirs; got %+v", list)
	}
}

func TestListEmptyRootReturnsNil(t *testing.T) {
	t.Parallel()
	// Not-yet-existent root should not error.
	list, err := List(filepath.Join(t.TempDir(), "not-created"))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("expected nil; got %v", list)
	}
}

func TestEnableDisableRoundTrip(t *testing.T) {
	t.Parallel()
	dstRoot := t.TempDir()
	src := t.TempDir()
	writePluginSource(t, src, "name: toggleable\nversion: 1.0.0\n")
	p, err := Install(src, dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Enabled {
		t.Error("newly-installed plugin should be enabled")
	}

	if err := Disable(dstRoot, "toggleable"); err != nil {
		t.Fatal(err)
	}
	list, _ := List(dstRoot)
	if len(list) != 1 || list[0].Enabled {
		t.Errorf("plugin should be disabled after Disable(); got %+v", list)
	}

	if err := Enable(dstRoot, "toggleable"); err != nil {
		t.Fatal(err)
	}
	list, _ = List(dstRoot)
	if len(list) != 1 || !list[0].Enabled {
		t.Errorf("plugin should be enabled after Enable(); got %+v", list)
	}
}

func TestEnableMissingPluginIsIdempotent(t *testing.T) {
	t.Parallel()
	// Enable on a plugin that doesn't exist is a no-op, not an error.
	// Rationale: scripted orchestration may call Enable preemptively
	// without knowing the state.
	if err := Enable(t.TempDir(), "ghost"); err != nil {
		t.Errorf("Enable missing should be no-op; got %v", err)
	}
}

func TestDisableRequiresInstalled(t *testing.T) {
	t.Parallel()
	// Disable is stricter — errors when the plugin isn't there.
	if err := Disable(t.TempDir(), "ghost"); err == nil {
		t.Error("Disable on missing plugin should error")
	}
}

func TestUninstallRemovesDirectory(t *testing.T) {
	t.Parallel()
	dstRoot := t.TempDir()
	src := t.TempDir()
	writePluginSource(t, src, "name: gone\nversion: 1.0.0\n")
	_, _ = Install(src, dstRoot)

	if err := Uninstall(dstRoot, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "gone")); !os.IsNotExist(err) {
		t.Errorf("plugin dir not removed: %v", err)
	}
}

func TestUninstallMissingErrors(t *testing.T) {
	t.Parallel()
	if err := Uninstall(t.TempDir(), "ghost"); err == nil {
		t.Error("uninstall missing should error")
	}
}

func TestPluginSkillDirs(t *testing.T) {
	t.Parallel()
	dstRoot := t.TempDir()
	src := t.TempDir()
	writePluginSource(t, src, "name: multi\nversion: 1.0.0\n")
	for _, s := range []string{"alpha", "bravo"} {
		sk := filepath.Join(src, SkillsSubdir, s)
		_ = os.MkdirAll(sk, 0o755)
		_ = os.WriteFile(filepath.Join(sk, "manifest.yaml"), []byte("name: "+s), 0o644)
	}
	p, _ := Install(src, dstRoot)

	dirs, err := p.SkillDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("len=%d", len(dirs))
	}
}

func TestIsURLLikeSource(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"/home/alice/plugin":           false,
		"./local":                      false,
		"git://example.com/p.git":      true,
		"https://example.com/p.tar.gz": true,
		"clawhub:web-search":           true,
		"github:alice/plugin":          true,
	}
	for src, want := range cases {
		if got := IsURLLikeSource(src); got != want {
			t.Errorf("%q: got %v want %v", src, got, want)
		}
	}
}
