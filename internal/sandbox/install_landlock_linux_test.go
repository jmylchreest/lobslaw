//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// A rw mount on a FILE must not ask for directory rights.
//
// /dev/null is mounted read-write by the device-node floor, and
// asking for make_dir on a character device is EINVAL — "using
// directory access rights on a regular file", as the kernel puts it
// through the library. One such rule fails the whole ruleset, and
// with it every command the sandbox was asked to run.
func TestFileMountsDropDirectoryRights(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := mountModeAccessFS(PolicyMount{Path: file, Read: true, Write: true})
	if got&^fileAccessFS != 0 {
		t.Errorf("access set for a file = %v; it carries rights only a directory has", got)
	}
	if got&landlock.AccessFSSet(llsyscall.AccessFSWriteFile) == 0 {
		t.Error("a rw file mount lost write_file, which is the point of it")
	}
}

// A directory keeps the full set — narrowing every mount to file
// rights would be a different bug with the same shape.
func TestDirectoryMountsKeepDirectoryRights(t *testing.T) {
	t.Parallel()
	got := mountModeAccessFS(PolicyMount{Path: t.TempDir(), Read: true, Write: true})
	if got&landlock.AccessFSSet(llsyscall.AccessFSMakeDir) == 0 {
		t.Error("a rw directory mount cannot create directories")
	}
	if got&landlock.AccessFSSet(llsyscall.AccessFSReadDir) == 0 {
		t.Error("a readable directory mount cannot be listed")
	}
}

// A path that does not exist keeps the full set: the rule carries
// IgnoreIfMissing and is dropped before the kernel sees it, and
// guessing "file" would silently narrow a directory created later.
func TestMissingPathsAreTreatedAsDirectories(t *testing.T) {
	t.Parallel()
	got := mountModeAccessFS(PolicyMount{
		Path: filepath.Join(t.TempDir(), "not-here"), Read: true, Write: true,
	})
	if got&landlock.AccessFSSet(llsyscall.AccessFSMakeDir) == 0 {
		t.Error("a missing path was narrowed to file rights")
	}
}

// The end-to-end guard: the device-node floor must install on a
// kernel that enforces landlock. This is the failure that was
// reported as "every shell_command dies, down to `id`".
func TestDeviceNodeFloorInstalls(t *testing.T) {
	if landlockABIVersion() == 0 {
		t.Skip("no landlock on this kernel")
	}
	if os.Getpid() == 1 {
		t.Skip("refusing to restrict pid 1")
	}
	// Installed in a subprocess would be cleaner, but the access-set
	// construction is what regressed and it is checkable here without
	// restricting the test process: build the rules, and assert every
	// one of them is expressible.
	for _, m := range []PolicyMount{
		{Path: "/dev/null", Read: true, Write: true},
		{Path: "/dev/zero", Read: true},
		{Path: "/dev/urandom", Read: true},
	} {
		access := mountModeAccessFS(m)
		if access == 0 {
			t.Errorf("%s resolved to no access at all", m.Path)
			continue
		}
		if access&^fileAccessFS != 0 {
			t.Errorf("%s carries directory rights (%v); landlock_add_rule rejects that",
				m.Path, access)
		}
	}
}
