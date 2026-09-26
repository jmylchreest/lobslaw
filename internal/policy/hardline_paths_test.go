package policy

import (
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
		"/var/lobslaw/data/raft.db",
		"/var/lobslaw/data/snapshots/snapshots/0001/state.bin",
		"/workspace/session.jwt",
	}
	for _, p := range denied {
		if v, _ := CheckPath(p); v != PathDenied {
			t.Errorf("CheckPath(%q) = %v, want PathDenied", p, v)
		}
	}
}

// The floor's raft-log entry must match the file raft actually writes,
// not a directory raft never creates.
func TestFloorDeniesRaftLog(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"/var/lobslaw/data/raft.db",
		"/home/u/somewhere/nested/raft.db",
	} {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			if v, _ := CheckPath(p); v != PathDenied {
				t.Errorf("CheckPath(%q) = %v, want PathDenied", p, v)
			}
		})
	}
}

// raft.NewFileSnapshotStore(filepath.Join(DataDir, SnapshotDir), ...)
// creates its own inner segment of the same name, so the segment
// sequence a written snapshot actually passes through is
// snapshots/snapshots/<id>, not a single snapshots directory.
func TestFloorDeniesRaftSnapshotStore(t *testing.T) {
	t.Parallel()
	denied := []string{
		"/var/lobslaw/data/snapshots/snapshots/0001/state.bin",
		"/var/lobslaw/data/snapshots/snapshots/0001/meta.json",
	}
	for _, p := range denied {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			if v, _ := CheckPath(p); v != PathDenied {
				t.Errorf("CheckPath(%q) = %v, want PathDenied", p, v)
			}
		})
	}
}

// A bare snapshots directory is common and must not be caught by the
// two-segment guard above.
func TestFloorAllowsOrdinarySnapshotsDirectory(t *testing.T) {
	t.Parallel()
	allowed := []string{
		"/home/u/projects/snapshots/notes.md",
		"/home/u/projects/snapshots",
		"/var/lobslaw/data/snapshots/README.md",
	}
	for _, p := range allowed {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			if v, _ := CheckPath(p); v != PathAllowed {
				t.Errorf("CheckPath(%q) = %v, want PathAllowed", p, v)
			}
		})
	}
}

// The guard is bound to the constants raft.go itself builds its paths
// from, so the two cannot drift apart.
func TestFloorRaftEntriesUseTheSharedConstants(t *testing.T) {
	t.Parallel()
	logPath := "/var/lobslaw/data/" + memory.RaftLogFile
	if v, _ := CheckPath(logPath); v != PathDenied {
		t.Errorf("CheckPath(%q) = %v, want PathDenied", logPath, v)
	}
	snapPath := "/var/lobslaw/data/" + memory.SnapshotDir + "/" + memory.RaftSnapshotStoreSegment + "/0001/state.bin"
	if v, _ := CheckPath(snapPath); v != PathDenied {
		t.Errorf("CheckPath(%q) = %v, want PathDenied", snapPath, v)
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
