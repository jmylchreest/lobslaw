package promptgen

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// commonUnixCommands are the everyday tools the LLM already knows
// from training-distribution corpora — listing them in the system
// prompt would waste tokens without helping. Anything on $PATH *not*
// in this set is specialty / operator-installed and DOES need
// advertising. Keep the list focused on stable, universally-available
// binaries across busybox + coreutils + common Linux distributions.
var commonUnixCommands = map[string]struct{}{
	// coreutils / busybox
	"ls": {}, "cat": {}, "cp": {}, "mv": {}, "rm": {}, "mkdir": {},
	"rmdir": {}, "touch": {}, "chmod": {}, "chown": {}, "ln": {},
	"readlink": {}, "realpath": {}, "stat": {}, "pwd": {}, "cd": {},
	"echo": {}, "printf": {}, "true": {}, "false": {}, "test": {},
	"env": {}, "export": {}, "unset": {}, "uname": {}, "hostname": {},
	"id": {}, "whoami": {}, "who": {}, "date": {}, "sleep": {},
	"head": {}, "tail": {}, "less": {}, "more": {}, "wc": {},
	"sort": {}, "uniq": {}, "cut": {}, "paste": {}, "tr": {},
	"tee": {}, "sed": {}, "awk": {}, "grep": {}, "egrep": {}, "fgrep": {},
	"find": {}, "xargs": {}, "which": {}, "type": {},
	// archives / compression
	"tar": {}, "gzip": {}, "gunzip": {}, "zip": {}, "unzip": {},
	"bzip2": {}, "xz": {},
	// networking / fetch
	"curl": {}, "wget": {}, "ping": {}, "nslookup": {}, "dig": {},
	"host": {}, "nc": {}, "netstat": {}, "ss": {},
	// process / system
	"ps": {}, "top": {}, "kill": {}, "killall": {}, "pgrep": {},
	"df": {}, "du": {}, "free": {}, "mount": {}, "umount": {},
	// text / json
	"jq": {}, "yq": {}, "diff": {}, "patch": {},
	// dev basics
	"git": {}, "make": {}, "go": {}, "python": {}, "python3": {},
	"node": {}, "npm": {}, "ruby": {}, "bash": {}, "sh": {}, "zsh": {},
}

var (
	pathSnapshotOnce    sync.Once
	pathSnapshotResult  []string
)

// discoverSpecialtyCommands walks each directory on $PATH, collects
// executable regular files, and returns those NOT in commonUnixCommands
// — i.e., the operator-installed specialty binaries the LLM wouldn't
// otherwise know about.
//
// Runs once per process (sync.Once) — output is cached in the
// package. Node restart refreshes; runtime $PATH changes won't.
// Intentional tradeoff: zero per-turn overhead vs. staleness on a
// change operators have to explicitly trigger.
func discoverSpecialtyCommands() []string {
	pathSnapshotOnce.Do(func() {
		pathSnapshotResult = enumerateSpecialtyPath(os.Getenv("PATH"))
	})
	return pathSnapshotResult
}

// enumerateSpecialtyPath is the testable core — takes a PATH string
// so unit tests can feed a controlled directory set.
func enumerateSpecialtyPath(rawPath string) []string {
	seen := make(map[string]struct{})
	specialty := make(map[string]struct{})

	for _, dir := range filepath.SplitList(rawPath) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if _, dup := seen[name]; dup {
				continue
			}
			// Reject obvious non-executables. Symlinks are allowed
			// (busybox applets + distribution tooling ship this way).
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.IsDir() {
				continue
			}
			if info.Mode()&0o111 == 0 {
				continue
			}
			seen[name] = struct{}{}
			if _, common := commonUnixCommands[name]; common {
				continue
			}
			specialty[name] = struct{}{}
		}
	}

	out := make([]string, 0, len(specialty))
	for name := range specialty {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// BuildEnvironment renders an "Environment" section enumerating the
// host OS + specialty binaries on $PATH. The LLM gets confirmation
// that typical Unix commands are available, plus an explicit list of
// operator-installed extras (rtk, bunx, himalaya, etc.) — no more
// guessing which tooling exists.
//
// Intentionally does NOT list commonUnixCommands — those are
// training-distribution knowledge. Only specialty binaries make the
// cut. If the specialty list is empty the "additionally available"
// line is elided entirely so we don't advertise an empty slot.
func BuildEnvironment(specialty []string) Section {
	var b strings.Builder
	b.WriteString("Typical Unix commands (coreutils, busybox, git, curl, jq, sed, awk, find, xargs, etc.) are available via the shell_command tool.\n")
	if len(specialty) > 0 {
		b.WriteString("\nAdditionally available on this machine: ")
		b.WriteString(strings.Join(specialty, ", "))
		b.WriteString(".\n")
	}
	b.WriteString("\nIf you need a command that isn't present, say so — don't fabricate one. shell_command is for LOCAL execution only; for online content use fetch_url or web_search.\n")
	return Section{Title: "Environment", Priority: PriorityContext, Body: b.String()}
}
