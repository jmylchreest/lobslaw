package compute

import (
	"strings"
	"testing"
)

// A grant names a command.
//
// So two things have to hold, and a break in either one means somebody
// approved something they were not shown. First, the key has to be
// stable under the ways a model spells the same command — otherwise an
// approved command is asked about again because the quoting moved, and
// the user learns to tap Always on everything. Second, and much worse,
// two DIFFERENT commands must never produce the same key, and the key
// must be exactly what the prompt displayed: a command that inherits a
// grant given for something else is a grant nobody gave.
//
// The refusals here are not a denylist of dangerous commands — the
// hardline floor does that. They are the commands with no stable
// identity: if what runs depends on the environment, or on more than
// one program, no key can describe it and the honest answer is to ask
// every time.

func TestWhitespaceAndQuotingVariantsShareAKey(t *testing.T) {
	t.Parallel()
	// Every spelling a model might reasonably emit for one command.
	// If these diverge, an approval is worth less than it looks.
	variants := []string{
		`git status --short`,
		`git  status   --short`,
		"git\tstatus --short",
		`git "status" --short`,
		`git 'status' --short`,
		`  git status --short  `,
	}
	want, ok := NormaliseCommand(variants[0])
	if !ok {
		t.Fatalf("NormaliseCommand(%q) refused", variants[0])
	}
	for _, v := range variants {
		got, ok := NormaliseCommand(v)
		if !ok {
			t.Errorf("NormaliseCommand(%q) refused; want %q", v, want)
			continue
		}
		if got != want {
			t.Errorf("NormaliseCommand(%q) = %q, want %q", v, got, want)
		}
	}
}

// The key is itself a command, so normalising it again must not move
// it. A key that drifts on re-normalisation would stop matching the
// rule it minted.
func TestAKeyIsStableUnderRenormalisation(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		`git status --short`,
		`grep '*.go'`,
		`sh -c 'rm -rf /tmp/build'`,
		`echo 'hello world'`,
		`ls -la /var/log`,
	} {
		once, ok := NormaliseCommand(cmd)
		if !ok {
			t.Errorf("NormaliseCommand(%q) refused", cmd)
			continue
		}
		twice, ok := NormaliseCommand(once)
		if !ok {
			t.Errorf("NormaliseCommand(%q) refused its own output %q", cmd, once)
			continue
		}
		if once != twice {
			t.Errorf("key drifted: %q -> %q -> %q", cmd, once, twice)
		}
	}
}

// The property the design rests on: what the user reads is what gets
// minted. Not derived from it, not truncated from it — identical.
func TestTheKeyIsWhatTheUserIsShown(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		`git status --short`,
		`ssh host uptime`,
		`sudo systemctl restart nginx`,
	} {
		params := map[string]string{"command": cmd}
		resource, grantable := shellGrant(params)
		if !grantable {
			t.Errorf("shellGrant(%q) was not grantable", cmd)
			continue
		}
		summary := ShellCommandSummary(params)
		if !strings.Contains(summary, resource) {
			t.Errorf("the prompt does not contain what would be minted:\n  summary  = %q\n  resource = %q",
				summary, resource)
		}
	}
}

func TestCommandsWithNoStableIdentityAreRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  string
	}{
		{"separator smuggling", `git status; rm -rf ~`},
		{"conjunction", `git status && rm -rf ~`},
		{"disjunction", `git status || rm -rf ~`},
		{"pipe", `git status | sh`},
		{"background", `git status & rm -rf ~`},
		{"command substitution", `git status $(rm -rf ~)`},
		{"backtick substitution", "git status `whoami`"},
		{"variable expansion", `git log $HOME`},
		{"expansion inside double quotes", `git log "$HOME"`},
		{"redirect out", `git status > /etc/passwd`},
		{"redirect in", `cat < /etc/shadow`},
		{"subshell", `(git status)`},
		{"newline", "git status\nrm -rf ~"},
		{"carriage return", "git status\rrm -rf ~"},
		{"line continuation", `git status \`},
		{"unterminated single quote", `git commit -m 'oops`},
		{"unterminated double quote", `git commit -m "oops`},
		{"glob", `rm *`},
		{"question glob", `rm file?`},
		{"bracket glob", `rm file[0-9]`},
		{"brace expansion", `rm file{a,b}`},
		{"tilde expansion", `rm ~/x`},
		{"env assignment prefix", `PATH=/tmp git status`},
		{"bidi override", "git\u202Estatus"},
		{"bidi isolate", "git\u2066status"},
		{"zero width space", "git\u200Bstatus"},
		{"right-to-left mark", "git\u200Fstatus"},
		{"byte order mark", "git\uFEFFstatus"},
		{"non-breaking space", "git\u00A0status"},
		{"nul byte", "git\x00status"},
		{"bell", "git\astatus"},
		{"empty", ``},
		{"whitespace only", `   `},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if key, ok := NormaliseCommand(tc.cmd); ok {
				t.Errorf("NormaliseCommand(%q) produced key %q; it has no stable identity and must be refused",
					tc.cmd, key)
			}
		})
	}
}

func TestInvalidUTF8IsRefused(t *testing.T) {
	t.Parallel()
	if key, ok := NormaliseCommand(string([]byte{0x67, 0x69, 0x74, 0xff, 0xfe})); ok {
		t.Errorf("invalid UTF-8 produced key %q", key)
	}
}

// A command nobody can read in full is one nobody can consent to, so
// it stays matchable but ungrantable.
func TestALongCommandIsMatchableButNotGrantable(t *testing.T) {
	t.Parallel()
	long := "echo " + strings.Repeat("a", shellKeyDisplayMax+50)
	resource, grantable := shellGrant(map[string]string{"command": long})
	if grantable {
		t.Error("a command past the display bound was offered as grantable")
	}
	if resource == UnclassifiedResource {
		t.Error("a long command lost its key; policy can no longer match on it")
	}
	if resource == "" {
		t.Error("a long command produced no resource at all")
	}
}

// The pairs that matter. Each is a safe command and a dangerous one
// that a careless key derivation would have collapsed together — the
// argv[0]+argv[1] scheme this design rejected would have given
// "ssh host" and "docker run" to both members of those rows.
func TestASafeAndAnUnsafeCommandNeverCollide(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{
		{`ssh host uptime`, `ssh host rm -rf /home/james`},
		{`docker run alpine ls`, `docker run --privileged alpine sh`},
		{`npm run build`, `npm run postinstall`},
		{`git status`, `git push --force`},
		{`ls /tmp`, `ls /etc`},
		{`sudo systemctl status nginx`, `sudo systemctl stop nginx`},
		{`kubectl get pods`, `kubectl delete pods --all`},
		// Homoglyph: Cyrillic "і" in place of ASCII "i". It reads as
		// the same command and must not inherit its grant.
		{`git status`, "g\u0456t status"},
	}
	for _, p := range pairs {
		a, aok := NormaliseCommand(p[0])
		b, bok := NormaliseCommand(p[1])
		if !aok || !bok {
			// A refusal is a safe outcome here — it means neither can
			// carry a grant at all.
			continue
		}
		if a == b {
			t.Errorf("%q and %q share the key %q", p[0], p[1], a)
		}
	}
}

// The sentinel namespace has to be unreachable, or a command could
// land on the resource an operator used to say "stop asking about
// compound commands".
func TestAKeyNeverReachesTheSentinel(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{
		`!unclassified`,
		`!unclassified --help`,
		`'!unclassified'`,
		`echo !unclassified`,
	} {
		key, ok := NormaliseCommand(cmd)
		if ok && key == UnclassifiedResource {
			t.Errorf("NormaliseCommand(%q) reached the sentinel", cmd)
		}
	}
	// And the resolver returns it only for commands it refused.
	if r, grantable := shellGrant(map[string]string{"command": `git status; rm -rf ~`}); r != UnclassifiedResource || grantable {
		t.Errorf("a refused command gave resource=%q grantable=%v, want the sentinel and false", r, grantable)
	}
}

// A quoted metacharacter is data, not syntax. Refusing these too would
// make `grep '*.go'` unapprovable for no safety gain, since the shell
// never expands it either.
func TestAQuotedMetacharacterIsData(t *testing.T) {
	t.Parallel()
	key, ok := NormaliseCommand(`grep '*.go' .`)
	if !ok {
		t.Fatal("a quoted glob was refused; it is literal to the shell too")
	}
	if !strings.Contains(key, `'*.go'`) {
		t.Errorf("key = %q; the quoted token should survive re-quoted", key)
	}
	// But the same characters unquoted must still be refused.
	if _, ok := NormaliseCommand(`grep *.go .`); ok {
		t.Error("an unquoted glob was accepted")
	}
	// And a key carrying a literal `*` is not grantable, because
	// ApprovalRules.Mint refuses a wildcard resource — offering the
	// button would report success and store nothing.
	if _, grantable := shellGrant(map[string]string{"command": `grep '*.go' .`}); grantable {
		t.Error("a key containing a wildcard was offered as grantable")
	}
}

func TestTheWorkingDirectoryIsPartOfTheGrantWhenSupplied(t *testing.T) {
	t.Parallel()
	bare, _ := shellGrant(map[string]string{"command": "git status"})
	inA, _ := shellGrant(map[string]string{"command": "git status", "cwd": "/repo-a"})
	inB, _ := shellGrant(map[string]string{"command": "git status", "cwd": "/repo-b"})

	if inA == inB {
		t.Errorf("the same command in two directories shares one key: %q", inA)
	}
	if inA == bare {
		t.Errorf("an explicit cwd did not change the key: %q", inA)
	}
	if !strings.Contains(inA, "/repo-a") {
		t.Errorf("key = %q; it does not name the directory", inA)
	}
	// Omitted is the common case and must stay clean, or every key
	// carries a path nobody asked about.
	if strings.Contains(bare, "cwd=") {
		t.Errorf("key = %q; an omitted cwd leaked into it", bare)
	}
}

func TestARelativeWorkingDirectoryIsNotGrantable(t *testing.T) {
	t.Parallel()
	// "../thing" resolves against whatever the process happens to be
	// in, so it does not identify a directory and cannot anchor a grant.
	if _, grantable := shellGrant(map[string]string{
		"command": "git status", "cwd": "../elsewhere",
	}); grantable {
		t.Error("a relative cwd was offered as grantable")
	}
}

// A prompt that paraphrases what will run cannot be answered.
func TestTheSummaryShowsTheCommandEvenWhenItCannotBeGranted(t *testing.T) {
	t.Parallel()
	summary := ShellCommandSummary(map[string]string{"command": `git status && make`})
	if !strings.Contains(summary, "git status && make") {
		t.Errorf("summary = %q; it does not show the command", summary)
	}
	if !strings.Contains(summary, "asked every time") {
		t.Errorf("summary = %q; it does not say the answer will not be remembered", summary)
	}
}

func TestTheSummaryFlagsNonASCII(t *testing.T) {
	t.Parallel()
	summary := ShellCommandSummary(map[string]string{"command": "g\u0456t status"})
	if !strings.Contains(summary, "non-ASCII") {
		t.Errorf("summary = %q; a homoglyph command was not flagged", summary)
	}
}

// The review found these. Each is a case where the key and the command
// were the same string to a reader and different operations to the
// shell — which is the one thing this design cannot tolerate, because
// the key is what the user reads AND what the minted rule stores, while
// the raw command is what actually runs.
func TestAKeyNeverNamesADifferentOperationThanItRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cmd  string
		why  string
	}{
		{
			"comment strips the tail",
			`git clean -fdx #-n`,
			"sh runs `git clean -fdx`; quoting the token would render a key naming a --dry-run that never happens",
		},
		{"comment at the end", `ls #foo`, "sh runs `ls`"},
		{"comment mid-command", `rm -rf /tmp/x #--dry-run`, "the guard flag is a comment"},
		{
			"bang negates and still runs",
			`! rm -rf /home/x`,
			"`! rm` runs rm; `'!' rm` is command-not-found — one key, two behaviours",
		},
		{"time is a reserved word", `time rm -rf /tmp/x`, "shell builtin vs /usr/bin/time"},
		{"quoted time is not", `'time' rm -rf /tmp/x`, "renders identically to the reserved word"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if key, ok := NormaliseCommand(tc.cmd); ok {
				t.Errorf("NormaliseCommand(%q) = %q, want refused — %s", tc.cmd, key, tc.why)
			}
		})
	}
}

// The exploit in full: approving the harmless form must not authorise
// the destructive one.
func TestTheCommentExploitCannotShareAKey(t *testing.T) {
	t.Parallel()
	harmless, okA := NormaliseCommand(`git clean -fdx '#-n'`)
	destructive, okB := NormaliseCommand(`git clean -fdx #-n`)
	if okA && okB && harmless == destructive {
		t.Fatalf("both forms share the key %q; an approval for the first authorises the second", harmless)
	}
}

// A cwd that cannot be shown safely must not be printed raw into the
// prompt — that is the display deception isInvisible exists to stop.
func TestAnUnprintableCwdIsNotRenderedIntoThePrompt(t *testing.T) {
	t.Parallel()
	params := map[string]string{"command": "rm -rf build", "cwd": "/home/x/safe\u202Eevil"}
	summary := ShellCommandSummary(params)
	if strings.ContainsRune(summary, '\u202E') {
		t.Errorf("a bidi override reached the prompt: %q", summary)
	}
	if !strings.Contains(summary, "non-ASCII") && !strings.Contains(summary, "withheld") {
		t.Errorf("summary = %q; the user is not told the directory was suppressed", summary)
	}
}
