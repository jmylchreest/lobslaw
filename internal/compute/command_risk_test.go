package compute

import (
	"context"
	"strings"
	"testing"
)

// The rows that matter most are the ones taken verbatim from a real
// session: an agent probing its sandbox produced eight confirmations in
// four minutes, each a wall of shell, each approved without being read.
// Those commands are the acceptance criteria for this classifier, so
// they are the first table below rather than a footnote.

func TestClassifyRiskObservedProbes(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		labels   []RiskLabel
		why      string
		programs []string
	}{
		{
			name: "tool inventory loop is unreadable",
			cmd: `id && echo "--- kernel ---" && uname -a && echo "--- tools ---" && ` +
				`for b in podman docker buildah skopeo git rustc go opencode; do ` +
				`printf '%-9s' "$b"; command -v "$b" || echo MISSING; done`,
			labels: L(LabelUnreadable),
			why:    "shell_keyword",
			// The readable part is still reported, which is the whole
			// point: the prompt can say what it DID understand.
			programs: []string{"id", "echo", "uname"},
		},
		{
			name: "capability probe reads only",
			cmd: `echo "--- os ---"; cat /etc/os-release | head -3; echo "--- caps ---"; ` +
				`grep -E 'CapEff|CapBnd|Seccomp' /proc/self/status; ` +
				`cat /proc/sys/user/max_user_namespaces 2>/dev/null; ` +
				`apt-get --version 2>&1 | head -1; df -h /`,
			labels: L(LabelReads),
		},
		{
			name: "writability probe writes AND deletes",
			cmd:  `touch /tmp/.w 2>/dev/null && echo "/tmp ok"; touch /workspace/.w 2>/dev/null && echo ok && rm /workspace/.w`,
			// Both, which is the whole point of a set: the tier this
			// replaced reported only the deletion and lost the fact
			// that it had also created files.
			labels: L(LabelDeletes, LabelWrites),
		},
		{
			name:   "sudo probe is privilege escalation",
			cmd:    `echo "--- sudo ---"; sudo -n true 2>&1; echo done`,
			labels: L(LabelPrivilege),
		},
		{
			name:   "egress probe reaches the network",
			cmd:    `echo "--- egress ---"; curl -sS -o /dev/null -w '%{http_code}' https://example.com`,
			labels: L(LabelNetwork),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRisk(tt.cmd)
			if !sameLabels(got.Labels, tt.labels) {
				t.Errorf("labels = %v, want %v (why %q, culprit %q)",
					got.Labels, tt.labels, got.Why, got.Culprit)
			}
			if tt.why != "" && got.Why != tt.why {
				t.Errorf("why = %q, want %q", got.Why, tt.why)
			}
			for _, want := range tt.programs {
				if !contains(got.Programs, want) {
					t.Errorf("programs = %v, want to contain %q", got.Programs, want)
				}
			}
		})
	}
}

// The tier must depend on what a command is POINTED AT, not only on
// what it is called. `rm` against a scratch file, `rm -rf /` and
// `rm -rf $DIR` are one program and three different operations.
func TestClassifyRiskTargetScope(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		labels []RiskLabel
		why    string
	}{
		{"rm under scratch is a write", "rm /tmp/probe.txt", L(LabelWrites), "scratch_path"},
		{"rm -rf under scratch is a write", "rm -rf /tmp/build/out", L(LabelWrites), "scratch_path"},
		{"rm of the scratch root itself is not", "rm -rf /tmp", L(LabelDeletes), ""},
		{"rm at the root is destructive", "rm -rf /", L(LabelPrivilege, LabelDeletes), "system_path"},
		{"rm of a system path is destructive", "rm -rf /etc/hosts", L(LabelPrivilege, LabelDeletes), "system_path"},
		{"rm with an interpolated target is unknown", "rm -rf $DIR", L(LabelUnreadable), "opaque_target"},
		{"rm with an interpolated target in quotes is unknown", `rm -rf "$DIR/build"`, L(LabelUnreadable), "opaque_target"},
		{"rm of a home path is unknown", "rm -rf ~/.ssh", L(LabelUnreadable), "opaque_target"},
		{"rm of a relative path stays destructive", "rm -rf build", L(LabelDeletes), ""},
		{"rm of a glob stays destructive", "rm -rf *", L(LabelDeletes), ""},
		{"traversal out of scratch does not borrow its scope", "rm -rf /tmp/../etc", L(LabelPrivilege, LabelDeletes), "system_path"},

		{"touch under scratch is a write", "touch /tmp/.w", L(LabelWrites), ""},
		{"touch in a system path is destructive", "touch /etc/nologin", L(LabelPrivilege, LabelWrites), "system_path"},
		{"copying FROM a system path is a write", "cp /etc/os-release /tmp/x", L(LabelWrites), ""},
		{"copying INTO a system path is destructive", "cp payload /usr/bin/ls", L(LabelPrivilege, LabelWrites), "system_path"},
		{"moving OUT of a system path is destructive", "mv /etc/shadow /tmp/x", L(LabelPrivilege, LabelWrites), "system_path"},
		{"chmod recursive on a system path is destructive", "chmod -R 777 /etc", L(LabelPrivilege, LabelWrites), "system_path"},
		{"chmod on a scratch path is a write", "chmod 644 /tmp/build/x", L(LabelWrites), ""},

		{"redirect to /dev/null is not a write", "echo hi > /dev/null", L(LabelReads), ""},
		{"fd duplication is not a write", "df -h / 2>&1", L(LabelReads), ""},
		{"redirect to a file is a write", "echo hi > /tmp/probe", L(LabelWrites), ""},
		{"redirect into a system path is destructive", "echo hi > /etc/passwd", L(LabelPrivilege, LabelWrites), "system_path"},
		{"append into a system path is destructive", "echo hi >> /etc/passwd", L(LabelPrivilege, LabelWrites), "system_path"},
		{"redirect to an interpolated path is unknown", "echo hi > $OUT", L(LabelUnreadable), "opaque_target"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRisk(tt.cmd)
			if !sameLabels(got.Labels, tt.labels) || got.Why != tt.why {
				t.Errorf("ClassifyRisk(%q) = %v/%q, want %v/%q",
					tt.cmd, got.Labels, got.Why, tt.labels, tt.why)
			}
		})
	}
}

// Everything that defeats static reading must produce unknown, and say
// which thing defeated it. A classifier that guesses here is worse than
// one that does not exist, because the guess is what auto-allows.
func TestClassifyRiskRefusals(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		reason string
	}{
		{"command substitution", "ls $(rm -rf /)", "command_substitution"},
		{"command substitution in quotes", `echo "$(rm -rf /)"`, "command_substitution"},
		{"backticks", "ls `rm -rf /`", "command_substitution"},
		{"variable in the command slot", "$CMD status", "variable_command"},
		{"variable in the subcommand slot", "git $ACTION", "variable_subcommand"},
		{"subshell", "(rm -rf /)", "subshell"},
		{"backslash escape", `rm -rf /tmp/a\ b`, "escaped_command"},
		{"shell loop", "for f in a b; do rm $f; done", "shell_keyword"},
		{"conditional", "if true; then rm -rf /; fi", "shell_keyword"},
		{"unrecognised program", "some-inhouse-tool --wipe", "unrecognised_command"},
		{"unrecognised subcommand", "git frobnicate", "unrecognised_subcommand"},
		{"interpreter runs unread code", `sh -c 'rm -rf /'`, "runs_unread_code"},
		{"environment assignment defeats the wrapper", "env PATH=/tmp ls", "unreadable_wrapper"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyRisk(tt.cmd)
			if !hasLabel(got.Labels, LabelUnreadable) {
				t.Fatalf("ClassifyRisk(%q).Labels = %v, want unreadable", tt.cmd, got.Labels)
			}
			if got.Why != tt.reason {
				t.Errorf("ClassifyRisk(%q).Why = %q, want %q", tt.cmd, got.Why, tt.reason)
			}
		})
	}
}

// A wrapper is not the command. `timeout 5 rm -rf /` is a deletion
// wearing a stopwatch, and reading only argv[0] would file it as a
// timeout.
func TestClassifyRiskWrappers(t *testing.T) {
	tests := []struct {
		cmd    string
		labels []RiskLabel
		why    string
	}{
		{"timeout 5 ls", L(LabelReads), ""},
		{"timeout -s KILL 5 rm -rf /", L(LabelPrivilege, LabelDeletes), "system_path"},
		{"nohup ls", L(LabelReads), ""},
		{"nice -n 10 grep x /etc/hosts", L(LabelReads), ""},
		{"env ls -l", L(LabelReads), ""},
		// Privilege is ADDED to what the wrapped command does, not
		// substituted for it: `sudo rm -rf /` is both.
		{"sudo -n true", L(LabelPrivilege), ""},
		{"sudo ls", L(LabelPrivilege), ""},
		{"sudo rm -rf /", L(LabelPrivilege, LabelDeletes), "system_path"},
		{"sudo some-inhouse-tool", L(LabelUnreadable), "unrecognised_command"},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			got := ClassifyRisk(tt.cmd)
			if !sameLabels(got.Labels, tt.labels) || got.Why != tt.why {
				t.Errorf("ClassifyRisk(%q) = %v/%q, want %v/%q",
					tt.cmd, got.Labels, got.Why, tt.labels, tt.why)
			}
		})
	}
}

// The shipped table, spot-checked where a program's tier depends on
// more than its name.
func TestClassifyRiskTable(t *testing.T) {
	tests := []struct {
		cmd    string
		labels []RiskLabel
	}{
		{"ls -la", L(LabelReads)},
		{"ls *.go", L(LabelReads)}, // a glob defeats a grant key, not a classification
		{"cat /etc/os-release", L(LabelReads)},
		{"git status --short", L(LabelReads)},
		{"git commit -m wip", L(LabelWrites)},
		{"git push origin main", L(LabelNetwork)},
		{"git clean -fdx", L(LabelDeletes)},
		{"git reset --hard HEAD", L(LabelDeletes, LabelWrites)},
		{"podman ps -a", L(LabelReads)},
		{"podman pull alpine", L(LabelNetwork)},
		{"podman rm -f web", L(LabelDeletes, LabelDisrupts)},
		{"podman run alpine sh", L(LabelUnreadable)},
		{"systemctl status nginx", L(LabelReads)},
		{"systemctl restart nginx", L(LabelDisrupts)},
		{"apt-get --version", L(LabelReads)},
		{"apt-get install curl", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"apt-get purge curl", L(LabelDeletes, LabelPrivilege)},
		{"sed s/a/b/ f", L(LabelReads)},
		{"sed -i s/a/b/ f", L(LabelWrites)},
		{"sed -i.bak s/a/b/ f", L(LabelWrites)},
		{"find . -name x", L(LabelReads)},
		{"find . -delete", L(LabelDeletes)},
		{"find . -exec rm {} ;", L(LabelUnreadable)},
		{"mount", L(LabelReads)},
		{"mount /dev/sda1 /mnt", L(LabelDisrupts)},
		{"dd if=/dev/zero of=/dev/sda", L(LabelDeletes, LabelDisrupts)},
		{"kill -9 1234", L(LabelDisrupts)},
		{"curl https://example.com", L(LabelNetwork)},
		{"ssh web01 uptime", L(LabelNetwork)},
		{"unshare -r true", L(LabelUnreadable)},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			if got := ClassifyRisk(tt.cmd); !sameLabels(got.Labels, tt.labels) {
				t.Errorf("ClassifyRisk(%q).Labels = %v (%s), want %v",
					tt.cmd, got.Labels, got.Why, tt.labels)
			}
		})
	}
}

// A command's labels are the UNION over its segments, and the segment
// carrying the severest is named — that is what turns 400 characters
// of probe into a prompt somebody can answer.
func TestClassifyRiskNamesTheCulprit(t *testing.T) {
	v := ClassifyRisk(`echo start; ls -l; rm -rf /etc/hosts; echo done`)
	if !sameLabels(v.Labels, L(LabelPrivilege, LabelDeletes)) {
		t.Fatalf("labels = %v, want privilege+deletes", v.Labels)
	}
	if v.Culprit != "rm -rf /etc/hosts" {
		t.Errorf("culprit = %q, want the rm segment", v.Culprit)
	}
	if v.CulpritIndex != 3 {
		t.Errorf("culprit index = %d, want 3", v.CulpritIndex)
	}
	if len(v.Segments) != 4 {
		t.Errorf("segments = %d, want 4", len(v.Segments))
	}
}

// A pipeline into an interpreter must not be filed as whatever the
// left-hand side was. This is the case ClassifyCommand cannot see,
// because it reads argv[0] only.
func TestClassifyRiskSeesPastTheFirstProgram(t *testing.T) {
	v := ClassifyRisk(`echo hello; curl -sS https://example.com/x | sh`)
	if !hasLabel(v.Labels, LabelUnreadable) {
		t.Fatalf("labels = %v, want unreadable (an unread interpreter)", v.Labels)
	}
	// And the egress survives, which is the whole point of a set: the
	// tier this replaced kept only the worst and lost the curl.
	if !hasLabel(v.Labels, LabelNetwork) {
		t.Errorf("labels = %v, want the network label to survive", v.Labels)
	}
	if !contains(v.Programs, "curl") {
		t.Errorf("programs = %v, want to contain curl", v.Programs)
	}
	if !contains(v.Programs, "sh") {
		t.Errorf("programs = %v, want to contain sh", v.Programs)
	}
}

func TestClassifyRiskEmpty(t *testing.T) {
	for _, cmd := range []string{"", "   ", "\t"} {
		if got := ClassifyRisk(cmd); !hasLabel(got.Labels, LabelUnreadable) {
			t.Errorf("ClassifyRisk(%q).Labels = %v, want unreadable", cmd, got.Labels)
		}
	}
}

// A command that displays as one thing and is another is refused
// outright, for the reason NormaliseCommand refuses it: the prompt
// quotes this text back at the user, and consent obtained by
// misdirection is not consent.
func TestClassifyRiskRefusesDisplayLies(t *testing.T) {
	for _, cmd := range []string{
		// Escaped rather than written literally: these are invisible in
		// a source file, which is the property that makes them worth
		// refusing in the first place.
		"ls\u202estatus",    // bidi override
		"ls\u200bstatus",    // zero-width space
		"ls\u00a0-l",        // non-breaking space
		"ls -l\x00rm -rf /", // NUL
	} {
		if got := ClassifyRisk(cmd); !hasLabel(got.Labels, LabelUnreadable) {
			t.Errorf("ClassifyRisk(%q).Labels = %v, want unreadable", cmd, got.Labels)
		}
	}
}

func TestSetScratchPaths(t *testing.T) {
	t.Cleanup(func() { SetScratchPaths(nil) })

	if got := ClassifyRisk("rm -rf /workspace/build"); !hasLabel(got.Labels, LabelDeletes) {
		t.Fatalf("before declaring the root: labels = %v, want deletes", got.Labels)
	}
	SetScratchPaths([]string{"/workspace"})
	if got := ClassifyRisk("rm -rf /workspace/build"); !sameLabels(got.Labels, L(LabelWrites)) {
		t.Errorf("after declaring the root: labels = %v, want writes", got.Labels)
	}
	// A relative root is dropped rather than honoured: it resolves
	// against whatever directory the process is in.
	SetScratchPaths([]string{"build"})
	if got := ClassifyRisk("rm -rf build"); !hasLabel(got.Labels, LabelDeletes) {
		t.Errorf("relative root: labels = %v, want deletes", got.Labels)
	}
}

func TestSetCommandRisksMergesOverTheDefaults(t *testing.T) {
	t.Cleanup(func() { SetCommandRisks(nil) })

	SetCommandRisks(map[string]CommandRiskRule{
		"terraform": {Labels: L(LabelDeletes)},
	})
	if got := ClassifyRisk("terraform apply"); !hasLabel(got.Labels, LabelDeletes) {
		t.Errorf("added entry: labels = %v, want deletes", got.Labels)
	}
	// The shipped table survives. Replacing it wholesale would mean one
	// in-house tool silently reclassified every command as unknown.
	if got := ClassifyRisk("ls -l"); !sameLabels(got.Labels, L(LabelReads)) {
		t.Errorf("shipped entry after a merge: labels = %v, want reads", got.Labels)
	}
	// An empty rule removes a shipped entry.
	SetCommandRisks(map[string]CommandRiskRule{"ls": {}})
	if got := ClassifyRisk("ls -l"); !hasLabel(got.Labels, LabelUnreadable) {
		t.Errorf("removed entry: labels = %v, want unreadable", got.Labels)
	}
}

func TestCommandLabelsContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, ok := CommandLabelsFrom(ctx); ok {
		t.Error("a bare context reports labels")
	}
	// An invalid label is dropped rather than carried: a rule
	// conditioned on labels must not match something nobody classified.
	if _, ok := CommandLabelsFrom(WithCommandLabels(ctx, L(RiskLabel("bananas")))); ok {
		t.Error("an invalid label was stored")
	}
	got, ok := CommandLabelsFrom(WithCommandLabels(ctx, L(LabelWrites)))
	if !ok || !sameLabels(got, L(LabelWrites)) {
		t.Errorf("CommandLabelsFrom = %v/%v, want writes/true", got, ok)
	}
}

// The gate is a SUBSET CHECK. Nothing is ranked, and an unreadable
// command is approvable by no set at all.
func TestVerdictApproved(t *testing.T) {
	t.Parallel()
	set := func(l ...RiskLabel) map[RiskLabel]bool {
		m := map[RiskLabel]bool{}
		for _, x := range l {
			m[x] = true
		}
		return m
	}
	tests := []struct {
		name     string
		labels   []RiskLabel
		approved map[RiskLabel]bool
		want     bool
	}{
		{"exactly approved", L(LabelReads), set(LabelReads), true},
		{"a subset of what is approved", L(LabelReads), set(LabelReads, LabelWrites), true},
		{"every label must be approved", L(LabelWrites, LabelNetwork), set(LabelReads, LabelWrites), false},
		{"both approved", L(LabelWrites, LabelDeletes), set(LabelWrites, LabelDeletes), true},
		// The shape a ranked tier could not express: deletes without
		// dragging network and disrupts along with it.
		{"a non-prefix set", L(LabelDeletes), set(LabelReads, LabelWrites, LabelDeletes), true},
		{"unreadable is never approved", L(LabelUnreadable), set(LabelUnreadable), false},
		{"nothing classified is not approval", nil, set(LabelReads), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := RiskVerdict{Labels: tt.labels}
			if got := v.Approved(tt.approved); got != tt.want {
				t.Errorf("Approved = %v, want %v", got, tt.want)
			}
		})
	}
}

// "reads" means reads AND NOTHING ELSE, so it never rides along beside
// a stronger label.
func TestReadsIsDroppedBesideOthers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		cmd  string
		want []RiskLabel
	}{
		{"sed -i s/a/b/ f", L(LabelWrites)},
		{"find . -delete", L(LabelDeletes)},
		{"sudo ls", L(LabelPrivilege)},
		{"uname -a", L(LabelReads)},
	} {
		if got := ClassifyRisk(tc.cmd); !sameLabels(got.Labels, tc.want) {
			t.Errorf("ClassifyRisk(%q) = %v, want %v", tc.cmd, got.Labels, tc.want)
		}
	}
}

// sameLabels compares two sets, order-insensitively.
func sameLabels(got, want []RiskLabel) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !hasLabel(got, w) {
			return false
		}
	}
	return true
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// The prompt leads with the classification and still shows the command
// verbatim and in full. Both halves matter: the headline is what makes
// a 300-character probe answerable in one glance, and the verbatim
// command is what makes the answer mean anything.
func TestShellCommandSummaryLeadsWithTheClassification(t *testing.T) {
	t.Parallel()
	const cmd = "echo start; rm -rf /etc/hosts"
	got := ShellCommandSummary(context.Background(), map[string]string{"command": cmd})

	head, rest, found := strings.Cut(got, "\n")
	if !found {
		t.Fatalf("summary has no headline line: %q", got)
	}
	if !strings.HasPrefix(head, "privilege") {
		t.Errorf("headline = %q, want it to lead with the labels", head)
	}
	if !strings.Contains(head, "rm -rf /etc/hosts") {
		t.Errorf("headline = %q, want it to name the step that caused the ask", head)
	}
	// Verbatim and in full, unchanged by the header above it.
	if !strings.Contains(rest, cmd) {
		t.Errorf("summary body = %q, want the command verbatim", rest)
	}
}

// A grantable command's summary must still contain the resource
// byte-for-byte: what the user reads is what gets minted, and a
// headline must not have disturbed that.
func TestShellCommandSummaryStillEchoesTheGrantKey(t *testing.T) {
	t.Parallel()
	params := map[string]string{"command": "git   status    --short"}
	target := ShellGrantResource(params)
	if !target.Grantable {
		t.Fatal("the fixture stopped being grantable")
	}
	got := ShellCommandSummary(context.Background(), params)
	if !strings.Contains(got, target.Resource) {
		t.Errorf("summary %q does not contain the grant key %q", got, target.Resource)
	}
}

// A read-only probe reports what it looked at, which is the line that
// replaces 400 characters of shell in the prompt.
func TestRiskHeadlineNamesThePrograms(t *testing.T) {
	t.Parallel()
	head := RiskHeadline(ClassifyRisk("id; uname -a; df -h /; cat /etc/os-release"))
	if !strings.HasPrefix(head, "reads") {
		t.Errorf("headline = %q, want it to lead with reads", head)
	}
	for _, prog := range []string{"id", "uname", "df", "cat"} {
		if !strings.Contains(head, prog) {
			t.Errorf("headline = %q, want it to mention %q", head, prog)
		}
	}
}

// Package managers, which the table barely covered and got wrong where
// it did. Five models across three vendors were polled on what these
// commands actually do; the rows below are that consensus after review,
// and the one place I declined to follow it is noted in the table.
func TestClassifyRiskPackageManagers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		cmd  string
		want []RiskLabel
	}{
		// Installing a system package needs root. The table said
		// "network" alone until every model polled said otherwise.
		{"apt-get install -y curl", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"apt-get remove curl", L(LabelDeletes, LabelPrivilege)},
		{"apt-get update", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"dnf install curl", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"apk add curl", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"apk del curl", L(LabelDeletes, LabelPrivilege)},

		// pacman takes its verb as a flag.
		{"pacman -Q", L(LabelReads)},
		{"pacman -Ss firefox", L(LabelReads)},
		{"pacman -S firefox", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"pacman -Syu", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"pacman -Rns firefox", L(LabelDeletes, LabelPrivilege)},
		{"pacman -Sc", L(LabelDeletes, LabelPrivilege)},
		{"pacman -U ./local.pkg.tar.zst", L(LabelPrivilege, LabelWrites)},

		// The AUR helpers are pacman plus a network hop, including on
		// a search, which pacman does locally and paru does not.
		{"yay -S firefox", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		// reads is dropped beside a stronger label, so an AUR search
		// reports the fact that matters: it reaches off the box.
		{"paru -Ss firefox", L(LabelNetwork)},
		{"yay -Q", L(LabelReads)},

		// Flag-driven, and now failing closed.
		{"rpm -qa", L(LabelReads)},
		{"rpm -i pkg.rpm", L(LabelPrivilege, LabelWrites)},
		{"rpm -e pkg", L(LabelDeletes, LabelPrivilege)},
		{"dpkg -l", L(LabelReads)},
		{"dpkg -i pkg.deb", L(LabelPrivilege, LabelWrites)},
		{"dpkg --purge pkg", L(LabelDeletes, LabelPrivilege)},

		// PRIVILEGE is the axis that separates these. emerge and snap
		// need root; brew, nix, gem and pipx install into a user prefix
		// and do not. The models were consistent about which is which.
		{"emerge www-client/firefox", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"snap install code", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"xbps-install -S firefox", L(LabelNetwork, LabelPrivilege, LabelWrites)},
		{"brew install wget", L(LabelNetwork, LabelWrites)},
		{"brew uninstall wget", L(LabelDeletes)},
		{"nix profile install nixpkgs#firefox", L(LabelNetwork, LabelWrites)},
		{"gem install rails", L(LabelNetwork, LabelWrites)},
		{"pipx install black", L(LabelNetwork, LabelWrites)},
		{"cargo install ripgrep", L(LabelNetwork, LabelWrites)},
		{"flatpak install flathub org.gimp.GIMP", L(LabelNetwork, LabelWrites)},
	}
	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			if got := ClassifyRisk(tt.cmd); !sameLabels(got.Labels, tt.want) {
				t.Errorf("ClassifyRisk(%q) = %v (%s), want %v",
					tt.cmd, got.Labels, got.Why, tt.want)
			}
		})
	}
}

// A flag nobody enumerated is unreadable, NOT the entry's base label.
//
// This is the whole reason these moved from Escalate to FlagSub.
// `pacman -Rdd` removes a package ignoring its dependencies; under the
// additive Escalate it inherited "reads" from the base entry and read
// as harmless.
func TestUnenumeratedFlagsFailClosed(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		"pacman -Qkk",
		"rpm --setperms pkg",
		"dpkg --force-all -i pkg.deb",
	} {
		got := ClassifyRisk(cmd)
		if !hasLabel(got.Labels, LabelUnreadable) {
			t.Errorf("ClassifyRisk(%q) = %v, want unreadable", cmd, got.Labels)
		}
		if got.Why != "unrecognised_flag" {
			t.Errorf("ClassifyRisk(%q).Why = %q, want unrecognised_flag", cmd, got.Why)
		}
	}
	// A flag that IS enumerated still classifies normally.
	if got := ClassifyRisk("pacman -Rdd firefox"); !sameLabels(got.Labels, L(LabelDeletes, LabelPrivilege)) {
		t.Errorf("pacman -Rdd = %v, want deletes+privilege", got.Labels)
	}
}
