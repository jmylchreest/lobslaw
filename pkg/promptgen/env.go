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

// Bounds on the specialty-binary scan. Without them a single hostile
// or merely unusual $PATH entry can dominate the system prompt on
// EVERY turn.
//
// Observed on WSL2: the Windows PATH is inherited, so
// /mnt/c/Windows/System32 lands on $PATH. drvfs synthesises mode 0777
// for every file it exposes, which defeats the executable-bit check
// below — every .dll, .dat and .nls in the directory reads as a
// binary. That produced a 124 KB (~31k token) Environment section,
// larger than the tool list and conversation history combined.
const (
	// maxDirEntries skips directories too large to be a curated tool
	// dir. /usr/local/bin has tens of entries; System32 has thousands.
	// Deliberately OS-agnostic — "implausibly many" generalises
	// better than special-casing /mnt.
	maxDirEntries = 512
	// maxSpecialtyCommands and maxSpecialtyBytes are the hard
	// backstop: whatever the filters miss, this section cannot grow
	// without bound.
	maxSpecialtyCommands = 150
	maxSpecialtyBytes    = 4096
)

// nonExecutableExtensions are suffixes that are never a runnable
// command from the shell tool's point of view, but which appear in
// bulk in Windows system directories visible through drvfs.
var nonExecutableExtensions = map[string]struct{}{
	".dll": {}, ".sys": {}, ".dat": {}, ".mui": {}, ".nls": {},
	".ini": {}, ".drv": {}, ".cpl": {}, ".ax": {}, ".tlb": {},
	".winmd": {}, ".efi": {}, ".msi": {}, ".mof": {}, ".rll": {},
	".log": {}, ".xml": {}, ".json": {}, ".txt": {}, ".png": {},
	// Windows executables. lobslaw's sandbox is Linux-only
	// (namespaces + Landlock + seccomp), so a .exe reachable through
	// a drvfs mount is not something shell_command can run —
	// advertising it invites the agent to try.
	".exe": {}, ".bat": {}, ".cmd": {}, ".com": {}, ".msc": {},
	".ps1": {}, ".ps1xml": {}, ".psd1": {}, ".psm1": {}, ".vbs": {},
}

// windowsMountPrefixes are paths where a Linux host sees a Windows
// filesystem. WSL mounts the Windows drives under /mnt and inherits
// the Windows PATH, and drvfs synthesises mode 0777 everywhere — so
// the executable-bit check that filters everywhere else is blind
// here. Nothing under these paths can run in the Linux sandbox.
var windowsMountPrefixes = []string{"/mnt/c/", "/mnt/d/", "/mnt/e/"}

// isForeignMount reports whether a $PATH directory lives on a mount
// whose contents can't be executed by the sandboxed shell tool.
func isForeignMount(dir string) bool {
	clean := filepath.Clean(dir) + "/"
	for _, prefix := range windowsMountPrefixes {
		if strings.HasPrefix(strings.ToLower(clean), prefix) {
			return true
		}
	}
	return false
}

// discoverSpecialtyCommands walks each directory on $PATH, collects
// executable regular files, and returns those NOT in commonUnixCommands
// — i.e., the operator-installed specialty binaries the LLM wouldn't
// otherwise know about.
//
// Runs once per process — output is cached. Node restart refreshes;
// runtime $PATH changes won't. Intentional tradeoff: zero per-turn
// overhead vs. staleness on a change operators have to explicitly
// trigger.
var discoverSpecialtyCommands = sync.OnceValue(func() []string {
	return enumerateSpecialtyPath(os.Getenv("PATH"))
})

// enumerateSpecialtyPath is the testable core — takes a PATH string
// so unit tests can feed a controlled directory set.
func enumerateSpecialtyPath(rawPath string) []string {
	seen := make(map[string]struct{})
	specialty := make(map[string]struct{})

	for _, dir := range filepath.SplitList(rawPath) {
		if dir == "" {
			continue
		}
		if isForeignMount(dir) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		// A directory this large is a system directory, not a place
		// an operator installed tooling. Scanning it yields noise at
		// best and thousands of Windows DLLs at worst.
		if len(entries) > maxDirEntries {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if _, dup := seen[name]; dup {
				continue
			}
			if _, skip := nonExecutableExtensions[strings.ToLower(filepath.Ext(name))]; skip {
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
	return capSpecialty(out)
}

// capSpecialty enforces the count and byte ceilings. Truncating is
// the right failure mode: this section is a hint, and a hint that
// costs 31k tokens a turn is worse than an incomplete one.
func capSpecialty(names []string) []string {
	if len(names) > maxSpecialtyCommands {
		names = names[:maxSpecialtyCommands]
	}
	total := 0
	for i, n := range names {
		total += len(n) + 2 // ", " separator
		if total > maxSpecialtyBytes {
			return names[:i]
		}
	}
	return names
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
	b.WriteString("\nWhen a needed command is missing, name what's missing and stop. shell_command runs LOCAL programs; for online content use fetch_url or web_search.\n")
	return Section{Title: "Environment", Priority: PriorityContext, Body: b.String()}
}
