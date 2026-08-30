package compute

import "testing"

// A command that reaches off the box must resolve to the same rule
// whichever tool ran it. `ssh web01 git pull` through shell_command and
// remote_ssh(remote=web01, command="git pull") are one operation, and
// an operator who granted one has granted the other.
func TestTheShellAndTheToolAgreeOnTheKey(t *testing.T) {
	t.Parallel()

	viaShell := ShellGrantResource(map[string]string{"command": "ssh web01 git pull"})
	viaTool := remoteGrant(e2eHosts, map[string]string{"remote": "web01", "command": "git pull"})

	if viaShell.Action != viaTool.Action {
		t.Errorf("actions differ: shell %q, tool %q", viaShell.Action, viaTool.Action)
	}
	if viaShell.Resource != viaTool.Resource {
		t.Errorf("keys differ:\n  shell %q\n  tool  %q", viaShell.Resource, viaTool.Resource)
	}
	if viaShell.Action != RemoteAction {
		t.Errorf("action = %q, want %q", viaShell.Action, RemoteAction)
	}
}

func TestHostExtractionShapes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, command  string
		wantAction     string
		wantHost       string
		wantClassified bool
	}{
		{"plain ssh", "ssh web01 uptime", RemoteAction, "web01", true},
		{"ssh with user", "ssh deploy@web01 uptime", RemoteAction, "web01", true},
		{"ssh with a valued flag", "ssh -p 2222 web01 uptime", RemoteAction, "web01", true},
		{"ssh with several flags", "ssh -i /k/id -o StrictHostKeyChecking=no web01 uptime", RemoteAction, "web01", true},
		{"absolute path to ssh", "/usr/bin/ssh web01 uptime", RemoteAction, "web01", true},
		{"scp upload", "scp ./f web01:/tmp/f", RemoteCopyAction, "web01", true},
		{"scp with user", "scp ./f deploy@web01:/tmp/f", RemoteCopyAction, "web01", true},
		{"rsync", "rsync -av ./d web01:/srv/d", RemoteCopyAction, "web01", true},
		{"curl", "curl https://api.example.com/v1", NetFetchAction, "api.example.com", true},
		{"wget", "wget https://files.example.com/x.tgz", NetFetchAction, "files.example.com", true},

		// Not classified: governed by shell:run as before.
		{"git", "git status", "", "", false},
		{"ls", "ls -la", "", "", false},

		// Classified but the host cannot be read out. Must stay
		// classified so the right rule is consulted, with no host so
		// nothing can be minted.
		{"ssh with only flags", "ssh -v", RemoteAction, "", true},
		{"curl with no url", "curl -sS", NetFetchAction, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tokens, ok := shellTokens(c.command)
			if !ok {
				t.Fatalf("shellTokens refused %q", c.command)
			}
			class, host, _, classified := ClassifyCommand(DefaultCommandClasses, tokens)
			if classified != c.wantClassified {
				t.Fatalf("classified = %v, want %v", classified, c.wantClassified)
			}
			if !classified {
				return
			}
			if class.Action != c.wantAction {
				t.Errorf("action = %q, want %q", class.Action, c.wantAction)
			}
			if host != c.wantHost {
				t.Errorf("host = %q, want %q", host, c.wantHost)
			}
		})
	}
}

// A host we could not read is confirmable but never grantable. A
// standing grant naming a host we guessed at is worse than asking
// again, because it is wrong silently and forever.
func TestAnUnreadableHostIsNotGrantable(t *testing.T) {
	t.Parallel()

	got := ShellGrantResource(map[string]string{"command": "ssh -v"})
	if got.Action != RemoteAction {
		t.Errorf("action = %q, want %q — the right rule must still be consulted", got.Action, RemoteAction)
	}
	if got.Grantable {
		t.Error("a call whose host could not be read was offered as grantable")
	}
	if got.Resource != UnclassifiedResource {
		t.Errorf("resource = %q, want the sentinel", got.Resource)
	}
}

// An approval is about a host. Granting `git pull` on web01 must not
// license it on db02 — the reason the host is IN the key rather than
// beside it.
func TestTheHostIsPartOfTheGrant(t *testing.T) {
	t.Parallel()

	a := remoteGrant(e2eHosts, map[string]string{"remote": "web01", "command": "git pull"})
	b := remoteGrant(e2eHosts, map[string]string{"remote": "db02", "command": "git pull"})
	if a.Resource == b.Resource {
		t.Errorf("the same command on two hosts produced one key: %q", a.Resource)
	}
}

// Sending a file and taking one are different permissions.
func TestCopyDirectionIsPartOfTheGrant(t *testing.T) {
	t.Parallel()

	up := remoteCopyGrant(e2eHosts, map[string]string{
		"remote": "web01", "direction": "upload", "remote_path": "/tmp/f", "local_path": "/w/f"})
	down := remoteCopyGrant(e2eHosts, map[string]string{
		"remote": "web01", "direction": "download", "remote_path": "/tmp/f", "local_path": "/w/f"})
	if up.Resource == down.Resource {
		t.Errorf("upload and download produced one key: %q", up.Resource)
	}
	if up.Action != RemoteCopyAction {
		t.Errorf("action = %q, want %q", up.Action, RemoteCopyAction)
	}
}

// An operator's table replaces the shipped one, and an empty action
// means "stop classifying this" — the way to disagree with a default
// without patching Go.
func TestAnOperatorCanStopClassifyingACommand(t *testing.T) {
	t.Parallel()

	tokens, _ := shellTokens("curl https://example.com")
	if _, _, _, classified := ClassifyCommand(map[string]CommandClass{
		"curl": {Action: "", HostFrom: HostFromURL},
	}, tokens); classified {
		t.Error("an empty action still classified the command")
	}
}

// e2eHosts is the lookup the gate supplies in production, standing in
// for the operator's [[remote]] blocks.
func e2eHosts(name string) (string, bool) {
	switch name {
	case "web01":
		return "web01", true
	case "db02":
		return "db02", true
	}
	return "", false
}
