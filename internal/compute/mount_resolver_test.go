package compute

import (
	"errors"
	"strings"
	"testing"
)

func TestParseMountMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in            string
		want          MountMode
		wantErr       bool
		wantCanonical string
	}{
		{"", MountMode{Read: true}, false, "r"},
		{"r", MountMode{Read: true}, false, "r"},
		{"ro", MountMode{Read: true}, false, "r"},
		{"rw", MountMode{Read: true, Write: true}, false, "rw"},
		{"RW", MountMode{Read: true, Write: true}, false, "rw"},
		{"rx", MountMode{Read: true, Exec: true}, false, "rx"},
		{"rwx", MountMode{Read: true, Write: true, Exec: true}, false, "rwx"},
		{"wx", MountMode{Read: true, Write: true, Exec: true}, false, "rwx"},
		{"q", MountMode{}, true, ""},
	}
	for _, tc := range cases {
		got, err := ParseMountMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseMountMode(%q) want error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMountMode(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMountMode(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.wantCanonical {
			t.Errorf("ParseMountMode(%q).String() = %q, want %q", tc.in, got.String(), tc.wantCanonical)
		}
	}
}

func TestMountResolverRejectsAbsolutePathOutsideAnyMount(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", MountMode{Read: true, Write: true}, nil)
	_, err := r.Resolve("/etc/passwd", MountMode{Read: true})
	if !errors.Is(err, ErrOutsideMounts) {
		t.Errorf("err=%v; want ErrOutsideMounts", err)
	}
}

func TestMountResolverAcceptsAbsolutePathInsideMount(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", MountMode{Read: true, Write: true}, nil)
	got, err := r.Resolve("/home/u/ws/notes.md", MountMode{Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/u/ws/notes.md" {
		t.Errorf("resolved: %q", got)
	}
}

func TestMountResolverAbsolutePathPicksLongestRoot(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("home", "/home", MountMode{Read: true}, nil)
	r.Register("ws", "/home/u/ws", MountMode{Read: true, Write: true}, nil)
	if _, err := r.Resolve("/home/u/ws/x.md", MountMode{Read: true, Write: true}); err != nil {
		t.Fatalf("write under ws should succeed: %v", err)
	}
	if _, err := r.Resolve("/home/other/x.md", MountMode{Read: true, Write: true}); !errors.Is(err, ErrModeDenied) {
		t.Errorf("write under home should be denied (mode r): %v", err)
	}
}

func TestMountResolverAbsolutePathSiblingNotPrefix(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("foo", "/srv/foo", MountMode{Read: true}, nil)
	if _, err := r.Resolve("/srv/foobar/x", MountMode{Read: true}); !errors.Is(err, ErrOutsideMounts) {
		t.Errorf("err=%v; want ErrOutsideMounts (sibling, not prefix)", err)
	}
}

func TestMountResolverRejectsUnknownLabel(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	_, err := r.Resolve("workspace/foo.md", MountMode{Read: true})
	if !errors.Is(err, ErrMountNotFound) {
		t.Errorf("err=%v; want ErrMountNotFound", err)
	}
}

func TestMountResolverResolvesSubpath(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", MountMode{Read: true, Write: true}, nil)
	got, err := r.Resolve("workspace/notes.md", MountMode{Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "/home/u/ws/notes.md" {
		t.Errorf("resolved: %q", got)
	}
}

func TestMountResolverRejectsTraversal(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", MountMode{Read: true, Write: true}, nil)
	_, err := r.Resolve("workspace/../../../etc/passwd", MountMode{Read: true})
	if !errors.Is(err, ErrPathEscape) {
		t.Errorf("err=%v; want ErrPathEscape", err)
	}
}

func TestMountResolverEnforcesReadOnly(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("snaps", "/data/snap", MountMode{Read: true}, nil)
	_, err := r.Resolve("snaps/a.db", MountMode{Read: true, Write: true})
	if !errors.Is(err, ErrModeDenied) {
		t.Errorf("err=%v; want ErrModeDenied", err)
	}
	got, err := r.Resolve("snaps/a.db", MountMode{Read: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "/data/snap/") {
		t.Errorf("read should succeed: %q", got)
	}
}

func TestMountResolverEnforcesExec(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("bin", "/lobslaw/bin", MountMode{Read: true, Exec: true}, nil)
	if _, err := r.Resolve("bin/foo", MountMode{Read: true, Exec: true}); err != nil {
		t.Fatalf("exec should succeed: %v", err)
	}
	if _, err := r.Resolve("bin/foo", MountMode{Read: true, Write: true}); !errors.Is(err, ErrModeDenied) {
		t.Errorf("write on rx mount should be denied: %v", err)
	}
}

func TestMountResolverAppliesExcludes(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("workspace", "/home/u/ws", MountMode{Read: true, Write: true}, []string{"secret.*"})
	_, err := r.Resolve("workspace/secret.key", MountMode{Read: true})
	if !errors.Is(err, ErrExcluded) {
		t.Errorf("err=%v; want ErrExcluded", err)
	}
}

func TestMountResolverLabelsSorted(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("charlie", "/c", MountMode{Read: true}, nil)
	r.Register("alpha", "/a", MountMode{Read: true}, nil)
	r.Register("bravo", "/b", MountMode{Read: true}, nil)
	labels := r.Labels()
	if strings.Join(labels, ",") != "alpha,bravo,charlie" {
		t.Errorf("labels order: %v", labels)
	}
}

func TestMountResolverMountsSnapshot(t *testing.T) {
	t.Parallel()
	r := NewMountResolver()
	r.Register("ws", "/ws", MountMode{Read: true, Write: true}, []string{"*.tmp"})
	r.Register("bin", "/bin", MountMode{Read: true, Exec: true}, nil)
	got := r.Mounts()
	if len(got) != 2 {
		t.Fatalf("Mounts() len = %d; want 2", len(got))
	}
	if got[0].Label != "bin" || got[1].Label != "ws" {
		t.Errorf("expected sorted labels, got %v", got)
	}
	if got[1].Mode.String() != "rw" || len(got[1].Excludes) != 1 {
		t.Errorf("ws mount snapshot wrong: %+v", got[1])
	}
}
