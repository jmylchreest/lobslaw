package policy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

// The merge. internalExcludes used to hold these; protectedPaths holds
// them now, so the fs builtins and shell_command consult one list.
func TestFloorCoversWhatTheFsListUsedTo(t *testing.T) {
	t.Parallel()
	denied := []string{
		"/var/lobslaw/data/state.db",
		"/var/lobslaw/data/state.db.lock",
		"/etc/lobslaw/certs/node-key.pem",
		"/etc/lobslaw/certs/ca.key",
		"/workspace/session.jwt",
	}
	for _, p := range denied {
		if v, _ := CheckPath(p); v != PathDenied {
			t.Errorf("CheckPath(%q) = %v, want PathDenied", p, v)
		}
	}
}

func TestFloorProtectsOnlyOwnedRaftPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	release, err := ProtectRaftPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	for _, tc := range []struct {
		name, path string
		want       PathVerdict
	}{
		{"root", root, PathDenied},
		{"log", filepath.Join(root, memory.RaftLogFile), PathDenied},
		{"snapshots", filepath.Join(root, memory.SnapshotDir), PathDenied},
		{"snapshot file", filepath.Join(root, memory.SnapshotDir, "snapshots", "0001", "state.bin"), PathDenied},
		{"snapshot parent file", filepath.Join(root, memory.SnapshotDir, "new"), PathDenied},
		{"unrelated log", filepath.Join(root, "project", memory.RaftLogFile), PathAllowed},
		{"unrelated snapshots", filepath.Join(root, "project", "snapshots", "snapshots", "notes.md"), PathAllowed},
		{"prefix sibling", root + "-other/raft.db", PathAllowed},
		{"snapshot prefix sibling", filepath.Join(root, memory.SnapshotDir+"-other", "notes.md"), PathAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := CheckPath(tc.path); got != tc.want {
				t.Errorf("CheckPath(%q) = %v, %v; want %v", tc.path, got, err, tc.want)
			}
		})
	}
}

func TestFloorResolvesRaftAliases(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(root, memory.SnapshotDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, memory.RaftLogFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	aliases := map[string]string{
		"log-alias":      filepath.Join(root, memory.RaftLogFile),
		"data-alias":     root,
		"snapshot-alias": filepath.Join("data", memory.SnapshotDir),
		"missing-alias":  filepath.Join(root, memory.SnapshotDir, "not-yet-created"),
	}
	for name, target := range aliases {
		if err := os.Symlink(target, filepath.Join(base, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Registration itself may use an alias.
	release, err := ProtectRaftPaths(filepath.Join(base, "data-alias"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	for _, relative := range []string{"log-alias", "data-alias/raft.db", "snapshot-alias/new/file", "missing-alias/file", "snapshot-alias/../raft.db"} {
		path := base + "/" + relative
		if got, err := CheckPath(path); got != PathDenied {
			t.Errorf("CheckPath(%q) = %v, %v", path, got, err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		relativePath, err := filepath.Rel(cwd, base)
		if err != nil {
			t.Fatal(err)
		}
		relativePath += "/" + relative
		if got, err := CheckPath(relativePath); got != PathDenied {
			t.Errorf("relative CheckPath(%q) = %v, %v", relativePath, got, err)
		}
		if err := CheckCommandPaths("cat " + path); err == nil {
			t.Errorf("command accepted %q", path)
		}
	}
}

func TestRaftPathRegistrationsHaveIndependentLifetimes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := ProtectRaftPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first)
	second, err := ProtectRaftPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second)
	path := filepath.Join(root, memory.RaftLogFile)
	first()
	first()
	if got, _ := CheckPath(path); got != PathDenied {
		t.Fatal("first release removed the other registration")
	}
	second()
	if got, _ := CheckPath(path); got != PathAllowed {
		t.Fatal("released registration still protects unrelated files")
	}
}

func TestFloorResolvesSecretAliasesAndCycles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, target := range map[string]string{"secret": filepath.Join(root, ".ssh", "id_rsa"), "cycle": "cycle"} {
		path := filepath.Join(root, name)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if got, err := CheckPath(path); got != PathDenied {
			t.Errorf("CheckPath(%q) = %v, %v", path, got, err)
		}
	}
}

// .git was in the fs list and is deliberately NOT in the floor. It was
// written for lobslaw's own data directory and caught every repository
// on the box, including .git/config — which reading a remote or a
// branch legitimately needs and which holds no secret.
func TestFloorDoesNotBlockOrdinaryGitFiles(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"/workspace/tasks/fix/.git/config",
		"/workspace/tasks/fix/.git/HEAD",
	} {
		if v, _ := CheckPath(p); v != PathAllowed {
			t.Errorf("CheckPath(%q) = %v, want PathAllowed — .git is not a secret", p, v)
		}
	}
	// The thing that actually holds credentials still is.
	if v, _ := CheckPath("/workspace/.git-credentials"); v != PathDenied {
		t.Errorf(".git-credentials = %v, want PathDenied", v)
	}
}

// The verdict model the old flat deny was masking. Nothing shared a
// pattern with a carve-out yet, so this was latent — this test is what
// stops it becoming live again.
func TestFloorStillDistinguishesConfirmFromDeny(t *testing.T) {
	t.Parallel()
	if v, _ := CheckPath("/home/u/.ssh/id_rsa"); v != PathDenied {
		t.Errorf("id_rsa = %v, want PathDenied", v)
	}
	if v, _ := CheckPath("/home/u/.ssh/config"); v != PathConfirm {
		t.Errorf("~/.ssh/config = %v, want PathConfirm — a carve-out must survive the merge", v)
	}
}

func TestFloorProtectsSymlinkedRaftStorageTargets(t *testing.T) {
	t.Parallel()
	root, target := t.TempDir(), t.TempDir()
	logTarget := filepath.Join(target, "log-data")
	snapshotTarget := filepath.Join(target, "snapshot-data")
	if err := os.WriteFile(logTarget, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(snapshotTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, dest := range map[string]string{memory.RaftLogFile: logTarget, memory.SnapshotDir: snapshotTarget} {
		if err := os.Symlink(dest, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	release, err := ProtectRaftPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	for _, path := range []string{logTarget, snapshotTarget, filepath.Join(snapshotTarget, "new", "state.bin"), filepath.Join(root, memory.RaftLogFile), filepath.Join(root, memory.SnapshotDir, "new")} {
		if got, err := CheckPath(path); got != PathDenied {
			t.Errorf("CheckPath(%q) = %v, %v", path, got, err)
		}
	}
}
