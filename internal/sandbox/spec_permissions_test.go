package sandbox

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func TestPolicySpecPreservesAccess(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"r", "rw", "rx", "rwx"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			p, err := (&PolicySpec{Paths: []string{dir + ":" + mode}}).ToPolicy()
			if err != nil {
				t.Fatal(err)
			}
			want := PolicyMount{Path: dir, Read: true, Write: mode == "rw" || mode == "rwx", Exec: mode == "rx" || mode == "rwx"}
			if !slices.Equal(p.Mounts, []PolicyMount{want}) || len(p.AllowedPaths) != 0 || len(p.ReadOnlyPaths) != 0 {
				t.Fatalf("permissions lost: %+v", p)
			}
		})
	}
}

func TestPolicyFileEnvironmentWhitelist(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, field string
		want        []string
	}{
		{"inherit", "", nil},
		{"empty", "env_whitelist = []", []string{}},
		{"explicit", `env_whitelist = ["PATH"]`, []string{"PATH"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writePolicyFile(t, filepath.Join(dir, "probe.toml"), fmt.Sprintf("name = %q\n%s\n", "probe", tc.field))
			result, err := LoadPolicyDir(dir, LoadOptions{})
			if err != nil {
				t.Fatal(err)
			}
			p := result.Policies["probe"]
			if p == nil {
				t.Fatalf("missing policy: %+v", result)
			}
			if !slices.Equal(p.EnvWhitelist, tc.want) || (p.EnvWhitelist == nil) != (tc.want == nil) {
				t.Fatalf("whitelist = %#v, want %#v", p.EnvWhitelist, tc.want)
			}
		})
	}
}

func TestWithPresetsPreservesExecute(t *testing.T) {
	t.Parallel()
	p, err := (Policy{}).WithPresets("system-libs")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Mounts) == 0 {
		t.Fatal("presets did not produce mounts")
	}
	for _, m := range p.Mounts {
		if !m.Read || !m.Exec || m.Write {
			t.Fatalf("runtime permissions: %+v", m)
		}
	}
}
