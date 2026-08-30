package policy

import "testing"

// The merge. internalExcludes used to hold these; protectedPaths holds
// them now, so the fs builtins and shell_command consult one list.
func TestFloorCoversWhatTheFsListUsedTo(t *testing.T) {
	t.Parallel()
	denied := []string{
		"/var/lobslaw/data/state.db",
		"/var/lobslaw/data/state.db.lock",
		"/etc/lobslaw/certs/node-key.pem",
		"/etc/lobslaw/certs/ca.key",
		"/var/lobslaw/data/.raft/0001.log",
		"/var/lobslaw/data/.snapshot/meta.json",
		"/workspace/session.jwt",
	}
	for _, p := range denied {
		if v, _ := CheckPath(p); v != PathDenied {
			t.Errorf("CheckPath(%q) = %v, want PathDenied", p, v)
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
