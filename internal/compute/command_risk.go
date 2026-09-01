package compute

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync/atomic"
	"unicode"
	"unicode/utf8"
)

// What a command DOES, as distinct from what it is CALLED.
//
// The per-command approval gate asks about every shell command,
// because until now nothing in the system had an opinion about what
// any of them do. That is defensible for `rm -rf /` and indefensible
// for `id && uname -a && df -h`, and the cost of not distinguishing
// them is not merely noise: an operator asked eight times in four
// minutes stops reading the prompt, and a confirmation answered
// reflexively launders consent rather than obtaining it. The comments
// in approval.go and shell_key.go both say so; this is the part that
// acts on it.
//
// So a command is classified into a risk tier, and an approval mode
// decides which tiers are worth asking about. The classification is
// STATIC and FAIL-CLOSED: a program the table does not name is
// unknown, an argument the classifier cannot read makes the whole
// segment unknown, and unknown never auto-allows in any mode.
//
// Deliberately NOT an embedding or a similarity score. `rm -rf
// /tmp/build` and `ls -l /tmp/build` are neighbours in every text
// embedding, and the only property that justifies not asking is
// soundness — a nearest neighbour does not have one. Where a model is
// wanted it answers a closed enum and is consulted separately; see
// command_risk_model.go.

// CommandRisk is the tier a command falls into.
//
// Named CommandRisk rather than RiskTier because types.RiskTier
// already classifies TOOLS by reversibility. That one is a property of
// a tool definition; this is a property of one invocation's argv, and
// conflating them would mean shell_command carried a single tier for
// every command it ever runs — which is the situation this replaces.
type CommandRisk string

const (
	// RiskRead inspects state and changes none of it.
	RiskRead CommandRisk = "read"
	// RiskWrite mutates local state in a way that is ordinarily
	// recoverable: creating, copying, appending, editing.
	RiskWrite CommandRisk = "write"
	// RiskNetwork reaches off the box. Separate from write because the
	// blast radius is somebody else's machine, and because egress is
	// the shape prompt injection wants.
	RiskNetwork CommandRisk = "network"
	// RiskDestructive removes data, kills processes, changes machine
	// state, or runs as root.
	RiskDestructive CommandRisk = "destructive"
	// RiskUnknown is what the classifier says when it cannot read the
	// command. The honest answer, and the one that always asks.
	RiskUnknown CommandRisk = "unknown"
)

// riskRank orders the tiers. A command's tier is the maximum over its
// segments, so the order decides which segment gets named in the
// prompt.
//
// Unknown ranks HIGHEST, above destructive. That looks odd until you
// state it as a question: given one segment we know deletes data and
// one we cannot read at all, which is the safer thing to tell the
// user? The unreadable one, because the readable one is bounded and
// the unreadable one is not.
var riskRank = map[CommandRisk]int{
	RiskRead:        1,
	RiskWrite:       2,
	RiskNetwork:     3,
	RiskDestructive: 4,
	RiskUnknown:     5,
}

// Rank exposes the ordering for callers that compare tiers.
func (r CommandRisk) Rank() int { return riskRank[r] }

// Valid reports whether r is one of the five. An unrecognised tier
// from config or from a model is discarded rather than trusted — the
// same reason Hint.Valid exists.
func (r CommandRisk) Valid() bool { return riskRank[r] != 0 }

// AtLeast returns the higher of the two tiers.
func (r CommandRisk) AtLeast(other CommandRisk) CommandRisk {
	if other.Rank() > r.Rank() {
		return other
	}
	return r
}

// Label is how the tier is written in a confirmation prompt.
func (r CommandRisk) Label() string {
	switch r {
	case RiskRead:
		return "read-only"
	case RiskWrite:
		return "writes"
	case RiskNetwork:
		return "network"
	case RiskDestructive:
		return "destructive"
	default:
		return "unclassified"
	}
}

// CommandRiskRule is how one program name is classified.
//
// Shaped like CommandClass in command_class.go: a compiled table an
// operator extends through config rather than a heuristic. The extra
// fields exist because a program name alone is not always enough —
// `git status` and `git clean -fdx` are the same program.
type CommandRiskRule struct {
	// Tier applies when nothing below does.
	Tier CommandRisk

	// Sub classifies by the first non-flag argument, for programs that
	// are really a family of commands: git, podman, systemctl, apt-get.
	// A subcommand the map does not name is UNKNOWN rather than Tier —
	// a git subcommand nobody classified could be anything.
	Sub map[string]CommandRisk

	// OperandTier applies when the segment carries at least one
	// non-flag operand. For programs that report when bare and act when
	// given something to act on: `mount` lists mounts, `mount /dev/sda1
	// /mnt` does not.
	OperandTier CommandRisk

	// Targets marks a program whose risk is decided by WHAT IT IS
	// POINTED AT rather than by its name.
	//
	// `rm /tmp/probe.txt`, `rm -rf /` and `rm -rf $DIR` are the same
	// program and three different operations, and a classifier that
	// calls all three "destructive" is telling the user nothing they
	// can act on — which is how a prompt becomes a reflex. So a
	// targeting program's operands are read as paths: a system path
	// raises the tier, a path under a declared scratch root lowers it
	// to ScratchTier, and a path whose value we cannot see makes the
	// segment unknown.
	Targets bool

	// ScratchTier is the tier when EVERY target is under a scratch
	// root. Only consulted when Targets is set, and only ever lower
	// than Tier — this is the de-escalation, and it is the only one in
	// the classifier.
	ScratchTier CommandRisk

	// TargetLast means only the final operand is written to and the
	// rest are read. `cp /etc/passwd /tmp/x` copies a system file to a
	// scratch one; scoping the source would report a change to the
	// machine that is not happening. `mv` is deliberately NOT in this
	// category — moving something out of /etc does change the machine.
	TargetLast bool

	// Escalate raises the tier when a token is present. Keys match a
	// token exactly, or as a prefix when they end in "*" — the same
	// wildcard convention policy patterns use, and the reason `sed
	// -i.bak` is caught alongside `sed -i`.
	Escalate map[string]CommandRisk

	// Reason overrides the tier's generic reason code, for entries
	// where it would say nothing. Every interpreter classifies as
	// unknown, and telling somebody `sh` is "unreadable" is true and
	// useless — "runs_unread_code" is the thing they can act on.
	Reason string
}

// DefaultCommandRisks is the shipped table.
//
// Incomplete on purpose and safe anyway: a program that is not here is
// RiskUnknown, which asks. Entries are added when somebody has thought
// about what the program does with every flag it might carry, not to
// make the table look finished.
//
// Operators extend or override it via [compute.command_risks].
var DefaultCommandRisks = map[string]CommandRiskRule{
	// Inspection. Nothing here writes anywhere with the flags shown;
	// the ones that CAN write carry an escalation.
	"ls": {Tier: RiskRead}, "dir": {Tier: RiskRead}, "vdir": {Tier: RiskRead},
	"cat": {Tier: RiskRead}, "head": {Tier: RiskRead}, "tail": {Tier: RiskRead},
	"wc": {Tier: RiskRead}, "nl": {Tier: RiskRead}, "tac": {Tier: RiskRead},
	"grep": {Tier: RiskRead}, "egrep": {Tier: RiskRead}, "fgrep": {Tier: RiskRead},
	"rg": {Tier: RiskRead}, "ag": {Tier: RiskRead},
	"stat": {Tier: RiskRead}, "file": {Tier: RiskRead}, "du": {Tier: RiskRead},
	"df": {Tier: RiskRead}, "id": {Tier: RiskRead}, "groups": {Tier: RiskRead},
	"whoami": {Tier: RiskRead}, "logname": {Tier: RiskRead},
	"uname": {Tier: RiskRead}, "hostname": {Tier: RiskRead}, "uptime": {Tier: RiskRead},
	"date": {Tier: RiskRead}, "cal": {Tier: RiskRead}, "locale": {Tier: RiskRead},
	"printenv": {Tier: RiskRead}, "echo": {Tier: RiskRead}, "printf": {Tier: RiskRead},
	"true": {Tier: RiskRead}, "false": {Tier: RiskRead}, "test": {Tier: RiskRead},
	"[": {Tier: RiskRead}, "seq": {Tier: RiskRead}, "yes": {Tier: RiskRead},
	"which": {Tier: RiskRead}, "command": {Tier: RiskRead}, "type": {Tier: RiskRead},
	"whereis": {Tier: RiskRead}, "hash": {Tier: RiskRead},
	"ps": {Tier: RiskRead}, "pgrep": {Tier: RiskRead}, "pidof": {Tier: RiskRead},
	"free": {Tier: RiskRead}, "vmstat": {Tier: RiskRead}, "iostat": {Tier: RiskRead},
	"lsblk": {Tier: RiskRead}, "lscpu": {Tier: RiskRead}, "lspci": {Tier: RiskRead},
	"lsusb": {Tier: RiskRead}, "lsmod": {Tier: RiskRead}, "lsns": {Tier: RiskRead},
	"getent": {Tier: RiskRead}, "getconf": {Tier: RiskRead}, "ulimit": {Tier: RiskRead},
	"dmesg": {Tier: RiskRead}, "journalctl": {Tier: RiskRead},
	"sha1sum": {Tier: RiskRead}, "sha256sum": {Tier: RiskRead}, "md5sum": {Tier: RiskRead},
	"cksum": {Tier: RiskRead}, "b2sum": {Tier: RiskRead},
	"diff": {Tier: RiskRead}, "cmp": {Tier: RiskRead}, "comm": {Tier: RiskRead},
	"sort": {Tier: RiskRead}, "uniq": {Tier: RiskRead}, "cut": {Tier: RiskRead},
	"paste": {Tier: RiskRead}, "join": {Tier: RiskRead}, "column": {Tier: RiskRead},
	"tr": {Tier: RiskRead}, "rev": {Tier: RiskRead}, "fold": {Tier: RiskRead},
	"od": {Tier: RiskRead}, "xxd": {Tier: RiskRead}, "hexdump": {Tier: RiskRead},
	"strings": {Tier: RiskRead}, "ldd": {Tier: RiskRead}, "nm": {Tier: RiskRead},
	"basename": {Tier: RiskRead}, "dirname": {Tier: RiskRead},
	"readlink": {Tier: RiskRead}, "realpath": {Tier: RiskRead}, "pwd": {Tier: RiskRead},
	"jq": {Tier: RiskRead}, "yq": {Tier: RiskRead},

	// Reads by default, writes when told to.
	"sed": {Tier: RiskRead, Escalate: map[string]CommandRisk{
		"-i*": RiskWrite, "--in-place*": RiskWrite,
	}},
	"find": {Tier: RiskRead, Escalate: map[string]CommandRisk{
		"-delete": RiskDestructive, "-exec": RiskUnknown, "-execdir": RiskUnknown,
		"-ok": RiskUnknown, "-okdir": RiskUnknown, "-fprint*": RiskWrite,
	}},
	"tar": {Tier: RiskRead, Escalate: map[string]CommandRisk{
		"-x*": RiskWrite, "--extract": RiskWrite, "-c*": RiskWrite, "--create": RiskWrite,
		"--delete": RiskDestructive,
	}},
	"unzip": {Tier: RiskWrite}, "zip": {Tier: RiskWrite}, "gzip": {Tier: RiskWrite},
	"gunzip": {Tier: RiskWrite}, "zcat": {Tier: RiskRead},
	// `mount` bare lists; `mount <device> <point>` acts.
	"mount":  {Tier: RiskRead, OperandTier: RiskDestructive},
	"umount": {Tier: RiskDestructive},
	"swapon": {Tier: RiskRead, OperandTier: RiskDestructive},

	// Local mutation, ordinarily recoverable. Targeting, so that
	// writing into /etc is not filed alongside writing into /tmp.
	"touch": {Tier: RiskWrite, Targets: true}, "mkdir": {Tier: RiskWrite, Targets: true},
	"mktemp":  {Tier: RiskWrite},
	"cp":      {Tier: RiskWrite, Targets: true, TargetLast: true},
	"mv":      {Tier: RiskWrite, Targets: true},
	"ln":      {Tier: RiskWrite, Targets: true},
	"install": {Tier: RiskWrite, Targets: true, TargetLast: true},
	"tee":     {Tier: RiskWrite, Targets: true},
	"mkfifo":  {Tier: RiskWrite, Targets: true},
	"patch":   {Tier: RiskWrite, Targets: true}, "split": {Tier: RiskWrite, Targets: true},
	"chmod": {Tier: RiskWrite, Targets: true, Escalate: map[string]CommandRisk{
		"-R*": RiskDestructive, "--recursive": RiskDestructive,
	}},
	"chown": {Tier: RiskWrite, Targets: true, Escalate: map[string]CommandRisk{
		"-R*": RiskDestructive, "--recursive": RiskDestructive,
	}},
	"chgrp": {Tier: RiskWrite, Targets: true, Escalate: map[string]CommandRisk{
		"-R*": RiskDestructive, "--recursive": RiskDestructive,
	}},

	// Reaching off the box. curl/wget/ssh and friends are ALSO in
	// DefaultCommandClasses, which is where the action comes from; they
	// are repeated here so that an operator who unclassifies one
	// (action = "") does not thereby make it look read-only.
	"curl": {Tier: RiskNetwork}, "wget": {Tier: RiskNetwork},
	"ssh": {Tier: RiskNetwork}, "scp": {Tier: RiskNetwork},
	"rsync": {Tier: RiskNetwork}, "rclone": {Tier: RiskNetwork},
	"sftp": {Tier: RiskNetwork}, "ftp": {Tier: RiskNetwork},
	"nc": {Tier: RiskNetwork}, "ncat": {Tier: RiskNetwork}, "socat": {Tier: RiskNetwork},
	"telnet": {Tier: RiskNetwork}, "ping": {Tier: RiskNetwork},
	"dig": {Tier: RiskNetwork}, "host": {Tier: RiskNetwork}, "nslookup": {Tier: RiskNetwork},
	"traceroute": {Tier: RiskNetwork}, "ip": {Tier: RiskRead, OperandTier: RiskRead},

	// Removal, machine state, privilege. The file-deleting ones target;
	// the disk-writing ones do not, because dd and mkfs are pointed at
	// devices and there is no scratch device.
	"rm":       {Tier: RiskDestructive, Targets: true, ScratchTier: RiskWrite},
	"rmdir":    {Tier: RiskDestructive, Targets: true, ScratchTier: RiskWrite},
	"shred":    {Tier: RiskDestructive, Targets: true, ScratchTier: RiskWrite},
	"truncate": {Tier: RiskDestructive, Targets: true, ScratchTier: RiskWrite},
	"dd":       {Tier: RiskDestructive}, "mkfs": {Tier: RiskDestructive},
	"fdisk": {Tier: RiskDestructive}, "parted": {Tier: RiskDestructive},
	"wipefs": {Tier: RiskDestructive}, "sfdisk": {Tier: RiskDestructive},
	"kill": {Tier: RiskDestructive}, "pkill": {Tier: RiskDestructive},
	"killall": {Tier: RiskDestructive}, "xkill": {Tier: RiskDestructive},
	"shutdown": {Tier: RiskDestructive}, "reboot": {Tier: RiskDestructive},
	"halt": {Tier: RiskDestructive}, "poweroff": {Tier: RiskDestructive},
	"iptables": {Tier: RiskDestructive}, "nft": {Tier: RiskDestructive},
	"useradd": {Tier: RiskDestructive}, "userdel": {Tier: RiskDestructive},
	"usermod": {Tier: RiskDestructive}, "groupadd": {Tier: RiskDestructive},
	"groupdel": {Tier: RiskDestructive}, "passwd": {Tier: RiskDestructive},
	"visudo": {Tier: RiskDestructive}, "crontab": {Tier: RiskDestructive},
	"insmod": {Tier: RiskDestructive}, "rmmod": {Tier: RiskDestructive},
	"modprobe": {Tier: RiskDestructive},

	// Families. A subcommand the map does not name is unknown.
	"git": {Tier: RiskRead, Sub: map[string]CommandRisk{
		"status": RiskRead, "log": RiskRead, "diff": RiskRead, "show": RiskRead,
		"branch": RiskRead, "describe": RiskRead, "blame": RiskRead, "config": RiskRead,
		"rev-parse": RiskRead, "ls-files": RiskRead, "ls-remote": RiskNetwork,
		"shortlog": RiskRead, "tag": RiskWrite, "stash": RiskWrite,
		"add": RiskWrite, "commit": RiskWrite, "checkout": RiskWrite,
		"switch": RiskWrite, "restore": RiskWrite, "merge": RiskWrite,
		"rebase": RiskWrite, "cherry-pick": RiskWrite, "revert": RiskWrite,
		"apply": RiskWrite, "am": RiskWrite, "init": RiskWrite, "worktree": RiskWrite,
		"clone": RiskNetwork, "fetch": RiskNetwork, "pull": RiskNetwork,
		"push": RiskNetwork, "remote": RiskNetwork, "submodule": RiskNetwork,
		"clean": RiskDestructive, "reset": RiskWrite, "gc": RiskDestructive,
		"prune": RiskDestructive, "filter-branch": RiskDestructive,
	}, Escalate: map[string]CommandRisk{
		"--hard": RiskDestructive, "--force": RiskDestructive, "-f": RiskDestructive,
	}},
	"podman": containerRisk, "docker": containerRisk, "nerdctl": containerRisk,
	"systemctl": {Tier: RiskRead, Sub: map[string]CommandRisk{
		"status": RiskRead, "show": RiskRead, "cat": RiskRead, "list-units": RiskRead,
		"list-unit-files": RiskRead, "is-active": RiskRead, "is-enabled": RiskRead,
		"start": RiskDestructive, "stop": RiskDestructive, "restart": RiskDestructive,
		"reload": RiskDestructive, "enable": RiskDestructive, "disable": RiskDestructive,
		"mask": RiskDestructive, "unmask": RiskDestructive, "daemon-reload": RiskDestructive,
	}},
	"apt-get": packageRisk, "apt": packageRisk, "apt-cache": {Tier: RiskRead},
	"dnf": packageRisk, "yum": packageRisk, "zypper": packageRisk,
	"pacman": {Tier: RiskUnknown}, "apk": packageRisk,
	"dpkg": {Tier: RiskRead, Escalate: map[string]CommandRisk{
		"-i": RiskWrite, "--install": RiskWrite,
		"-r": RiskDestructive, "--remove": RiskDestructive, "--purge": RiskDestructive,
	}},
	"rpm": {Tier: RiskRead, Escalate: map[string]CommandRisk{
		"-i": RiskWrite, "--install": RiskWrite,
		"-e": RiskDestructive, "--erase": RiskDestructive,
	}},
	"pip": pipRisk, "pip3": pipRisk,
	"npm": {Tier: RiskUnknown, Sub: map[string]CommandRisk{
		"ls": RiskRead, "list": RiskRead, "view": RiskNetwork, "outdated": RiskNetwork,
		"install": RiskNetwork, "ci": RiskNetwork, "publish": RiskNetwork,
		"uninstall": RiskWrite, "prune": RiskWrite,
	}},

	// Named so the prompt can say WHY rather than "not in the table".
	// Each of these runs code the classifier has not read.
	"sh": runsCode, "bash": runsCode, "zsh": runsCode, "dash": runsCode,
	"ksh": runsCode, "fish": runsCode, "eval": runsCode, "exec": runsCode,
	"source": runsCode, ".": runsCode,
	"xargs": runsCode, "parallel": runsCode, "watch": runsCode,
	"awk": runsCode, "gawk": runsCode, "mawk": runsCode, "perl": runsCode,
	"python": runsCode, "python3": runsCode, "ruby": runsCode, "php": runsCode,
	"node": runsCode, "deno": runsCode, "bun": runsCode,
	"make": runsCode, "cmake": runsCode, "cargo": runsCode, "go": runsCode,
	"gcc": runsCode, "cc": runsCode, "rustc": runsCode, "javac": runsCode,
	"unshare": newNamespace, "nsenter": newNamespace, "chroot": newNamespace,
}

// containerRisk is shared by the container CLIs, which are the same
// verbs with different names on the front.
var containerRisk = CommandRiskRule{Tier: RiskRead, Sub: map[string]CommandRisk{
	"ps": RiskRead, "images": RiskRead, "image": RiskRead, "inspect": RiskRead,
	"logs": RiskRead, "info": RiskRead, "version": RiskRead, "top": RiskRead,
	"port": RiskRead, "diff": RiskRead, "stats": RiskRead,
	"pull": RiskNetwork, "push": RiskNetwork, "login": RiskNetwork, "search": RiskNetwork,
	"build": RiskWrite, "commit": RiskWrite, "tag": RiskWrite, "save": RiskWrite,
	"load": RiskWrite, "create": RiskWrite, "cp": RiskWrite,
	"run": RiskUnknown, "exec": RiskUnknown, "start": RiskUnknown, "attach": RiskUnknown,
	"rm": RiskDestructive, "rmi": RiskDestructive, "kill": RiskDestructive,
	"stop": RiskDestructive, "prune": RiskDestructive, "system": RiskDestructive,
	"volume": RiskDestructive, "network": RiskDestructive,
}}

// packageRisk covers the distro package managers, which all reach the
// network to install and remove things locally to uninstall.
var packageRisk = CommandRiskRule{Tier: RiskRead, Sub: map[string]CommandRisk{
	"list": RiskRead, "show": RiskRead, "search": RiskRead, "policy": RiskRead,
	"info": RiskRead, "depends": RiskRead,
	"update": RiskNetwork, "install": RiskNetwork, "upgrade": RiskNetwork,
	"dist-upgrade": RiskNetwork, "download": RiskNetwork, "source": RiskNetwork,
	"remove": RiskDestructive, "purge": RiskDestructive, "autoremove": RiskDestructive,
	"erase": RiskDestructive, "clean": RiskDestructive,
}}

// pipRisk is the python package manager, whose install reaches out and
// whose uninstall does not.
var pipRisk = CommandRiskRule{Tier: RiskRead, Sub: map[string]CommandRisk{
	"list": RiskRead, "show": RiskRead, "freeze": RiskRead, "check": RiskRead,
	"config":  RiskRead,
	"install": RiskNetwork, "download": RiskNetwork, "wheel": RiskNetwork,
	"uninstall": RiskDestructive,
}}

// runsCode is every interpreter, build tool and re-execing wrapper.
//
// Unknown rather than a tier of their own: what `bash -c "$X"` does is
// whatever X does, and there is no honest tier for that. Naming them
// here rather than letting them fall through to "unrecognised" is what
// lets the prompt say "runs code this classifier has not read".
var runsCode = CommandRiskRule{Tier: RiskUnknown, Reason: "runs_unread_code"}

// newNamespace covers unshare/nsenter/chroot: the program that follows
// runs somewhere with different rules, so reading its argv here would
// describe the wrong thing.
var newNamespace = CommandRiskRule{Tier: RiskUnknown, Reason: "runs_in_new_namespace"}

// reasonFor names the tier's cause in the vocabulary the prompt and
// the model verdict share. Not prose — a closed set, so that what the
// user reads is generated the same way every time.
var reasonFor = map[CommandRisk]string{
	RiskRead:        "reads_only",
	RiskWrite:       "mutates_files",
	RiskNetwork:     "network_egress",
	RiskDestructive: "deletes_or_changes_machine_state",
	RiskUnknown:     "unreadable",
}

// pathScope is what a target path is part of.
type pathScope int

const (
	// scopeOrdinary is a path that is neither scratch nor system: the
	// project checkout, a data directory, somebody's work.
	scopeOrdinary pathScope = iota
	// scopeScratch is a declared throwaway root. Deleting inside one is
	// a write, not a loss.
	scopeScratch
	// scopeSystem is the machine itself.
	scopeSystem
	// scopeOpaque is a path whose value we cannot see — an expansion, a
	// substitution. `rm -rf $DIR` is not `rm -rf /tmp/x` and must never
	// be classified as though it were.
	scopeOpaque
)

// systemRoots are the paths where a write is a change to the machine
// rather than to somebody's files.
//
// Prefix-matched after cleaning, so /etc/systemd/system/x.service is
// system and /etcetera is not. This is NOT the hardline floor and does
// not pretend to be — the floor is enforced in internal/policy and is
// the only unpromptable layer. This list decides what a PROMPT SAYS
// and, in trusted mode, what is worth stopping for.
var systemRoots = []string{
	"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot",
	"/dev", "/proc", "/sys", "/run", "/root", "/opt", "/srv",
	"/var", "/home", "/mnt", "/media",
}

// defaultScratchPaths are the throwaway roots every deployment has.
//
// A deployment that mounts a workspace adds it through
// [compute.shell_approval] scratch_paths; nothing is assumed about
// /workspace or the cwd, because guessing that a directory is
// disposable is the one error in this file that loses data.
var defaultScratchPaths = []string{"/tmp", "/var/tmp"}

// activeScratchPaths is the operator's list, when there is one.
var activeScratchPaths atomic.Pointer[[]string]

// SetScratchPaths installs the roots under which deletion is a write
// rather than a loss. Empty restores the defaults.
func SetScratchPaths(paths []string) {
	clean := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		// Absolute only. A relative scratch root resolves against
		// whatever directory the process happens to be in, which is
		// exactly the ambiguity a downgrade must not rest on.
		if strings.HasPrefix(p, "/") {
			clean = append(clean, cleanPath(p))
		}
	}
	if len(clean) == 0 {
		activeScratchPaths.Store(nil)
		return
	}
	activeScratchPaths.Store(&clean)
}

// ActiveScratchPaths returns the roots in force.
func ActiveScratchPaths() []string {
	if p := activeScratchPaths.Load(); p != nil {
		return *p
	}
	return defaultScratchPaths
}

// pathScopeOf reads one operand as a path.
//
// A caveat worth stating out loud: this reads what is WRITTEN, not
// what it resolves to. A symlink at /tmp/x pointing into /etc makes
// `rm /tmp/x/conf` look like scratch. That is tolerable because the
// scratch downgrade only ever moves destructive to write, and write
// still asks under every mode except trusted — so the downgrade cannot
// by itself cause anything to run unasked in the shipped default.
func pathScopeOf(tok riskToken) pathScope {
	text := tok.text
	if tok.expands || strings.Contains(text, "$") {
		return scopeOpaque
	}
	if text == "" {
		return scopeOrdinary
	}
	// "~" is the home directory and "~/.ssh" is not scratch; treat the
	// whole shape as opaque rather than guessing whose home it is.
	if strings.HasPrefix(text, "~") {
		return scopeOpaque
	}
	if !strings.HasPrefix(text, "/") {
		// Relative to a working directory we may not have been told
		// about. Ordinary: not scratch (so nothing is downgraded) and
		// not system (so nothing is escalated on a guess).
		return scopeOrdinary
	}
	p := cleanPath(text)
	if p == "/" {
		return scopeSystem
	}
	// Scratch first: /var/tmp is under /var, and the more specific
	// declaration is the one that meant something.
	for _, root := range ActiveScratchPaths() {
		// Strictly under, never equal: `rm -rf /tmp` empties the
		// scratch root itself, which is a different act from deleting
		// something in it.
		if strings.HasPrefix(p, root+"/") {
			return scopeScratch
		}
	}
	for _, root := range systemRoots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return scopeSystem
		}
	}
	return scopeOrdinary
}

// cleanPath normalises a path for prefix comparison without touching
// the filesystem. Traversal is resolved textually, so
// /tmp/../etc/passwd is /etc/passwd and does not borrow /tmp's scope.
func cleanPath(p string) string {
	// A trailing glob segment is kept as a literal: /tmp/build* stays
	// under /tmp, and a bare "*" stays relative, which is what makes
	// `rm -rf *` ordinary rather than scratch.
	cleaned := path.Clean(p)
	if cleaned == "." {
		return p
	}
	return cleaned
}

// applyTargets adjusts a tier by what the command is pointed at.
//
// The three answers, in the order they are decided:
//   - any opaque target: unknown. We cannot say what `rm -rf $DIR`
//     removes, and a tier is a claim.
//   - any system target: at least destructive, whatever the program.
//     `cp payload /usr/bin/ls` is not a copy, it is a takeover.
//   - every target scratch, and the rule offers a ScratchTier: that.
func applyTargets(tier CommandRisk, rule CommandRiskRule, operands []riskToken, wroteTo []riskToken) (CommandRisk, string) {
	targets := make([]riskToken, 0, len(operands)+len(wroteTo))
	targets = append(targets, operands...)
	targets = append(targets, wroteTo...)
	if len(targets) == 0 {
		return tier, reasonFor[tier]
	}

	allScratch := true
	for _, t := range targets {
		switch pathScopeOf(t) {
		case scopeOpaque:
			return RiskUnknown, "opaque_target"
		case scopeSystem:
			return tier.AtLeast(RiskDestructive), "system_path"
		case scopeScratch:
		default:
			allScratch = false
		}
	}
	if allScratch && rule.ScratchTier != "" && rule.ScratchTier.Rank() < tier.Rank() {
		return rule.ScratchTier, "scratch_path"
	}
	return tier, reasonFor[tier]
}

// wrapperSpec describes a program that runs another program.
//
// Unwrapped rather than classified on its own, because `timeout 5 rm
// -rf /` is a deletion wearing a stopwatch. Getting this wrong in the
// permissive direction is the one mistake that matters, so a wrapper
// whose real command cannot be located yields unknown.
type wrapperSpec struct {
	// valueFlags consume the token after them.
	valueFlags map[string]bool
	// skipOperands is how many non-flag arguments come before the
	// command. `timeout 5 ls` has one; `nohup ls` has none.
	skipOperands int
	// root means the wrapped command runs with different privileges,
	// so the result is raised to at least destructive.
	root bool
	// refuseAssign means a VAR=value operand makes the command
	// unreadable — it runs in an environment we did not read, for the
	// reason NormaliseCommand refuses an assignment prefix.
	refuseAssign bool
}

var wrapperCommands = map[string]wrapperSpec{
	"sudo": {root: true, valueFlags: map[string]bool{
		"-u": true, "-g": true, "-p": true, "-C": true, "-h": true,
		"-r": true, "-t": true, "-U": true, "-D": true,
	}, refuseAssign: true},
	"doas":    {root: true, valueFlags: map[string]bool{"-u": true, "-C": true}},
	"pkexec":  {root: true, valueFlags: map[string]bool{"--user": true}},
	"timeout": {skipOperands: 1, valueFlags: map[string]bool{"-s": true, "--signal": true, "-k": true, "--kill-after": true}},
	"nice":    {valueFlags: map[string]bool{"-n": true, "--adjustment": true}},
	"ionice":  {valueFlags: map[string]bool{"-c": true, "-n": true, "-p": true}},
	"nohup":   {},
	"stdbuf":  {valueFlags: map[string]bool{"-i": true, "-o": true, "-e": true}},
	"env":     {refuseAssign: true},
	"setsid":  {},
}

// RiskSegment is one command in a compound command line.
type RiskSegment struct {
	// Raw is the segment as written, so the prompt can quote the exact
	// part that caused the ask rather than the whole line.
	Raw string `json:"raw"`
	// Program is the program name as invoked, with any path stripped.
	Program string `json:"program,omitempty"`
	// Via is the privilege wrapper the program ran under — sudo, doas,
	// pkexec — when there was one. Named separately because a headline
	// reading "destructive · true" for `sudo -n true` describes the
	// wrong half of what is happening.
	Via  string      `json:"via,omitempty"`
	Tier CommandRisk `json:"tier"`
	// Reason is a code from the closed set in reasonFor, or one of the
	// unreadable codes. Never free text.
	Reason string `json:"reason"`
}

// RiskVerdict is the classification of a whole command line.
type RiskVerdict struct {
	Tier CommandRisk
	// Programs is every program named, in order, without repeats. This
	// is what makes a 400-character probe legible in one line.
	Programs []string
	Segments []RiskSegment
	// Reason is the culprit segment's reason.
	Reason string
	// Culprit is the segment that set the tier, and CulpritIndex its
	// 1-based position. Empty and 0 when there is no single one.
	Culprit      string
	CulpritIndex int
	// Unreadable counts segments the classifier could not read, which
	// is what the prompt reports when the tier is unknown.
	Unreadable int
	// FromModel records that a configured model moved the tier. Display
	// only; the tier itself is already the final answer.
	FromModel bool
}

// ClassifyRisk reads a command line and says what it does.
//
// Never returns an error: an input it cannot read is a verdict of
// RiskUnknown, which is a real answer and the one that asks.
func ClassifyRisk(raw string) RiskVerdict {
	cmd := strings.TrimSpace(raw)
	if cmd == "" || !utf8.ValidString(cmd) {
		return RiskVerdict{Tier: RiskUnknown, Reason: "unreadable"}
	}
	segs, ok := splitRiskSegments(cmd)
	if !ok || len(segs) == 0 {
		return RiskVerdict{Tier: RiskUnknown, Reason: "unreadable"}
	}

	table := ActiveCommandRisks()
	v := RiskVerdict{Tier: RiskRead}
	seen := map[string]bool{}
	for _, seg := range segs {
		rs := classifyRiskSegment(seg, table)
		if rs.Tier == "" {
			continue // an empty segment: a trailing ";" or a stray "&&"
		}
		v.Segments = append(v.Segments, rs)
		if rs.Tier == RiskUnknown {
			v.Unreadable++
		}
		// The wrapper first, then the program it ran: `sudo`, `true`.
		// A shell keyword is not a program and listing `for`, `do`,
		// `done` alongside `id` and `uname` turns the one legible line
		// in the prompt back into noise.
		for _, name := range []string{rs.Via, rs.Program} {
			if name == "" || seen[name] || rs.Reason == "shell_keyword" {
				continue
			}
			seen[name] = true
			v.Programs = append(v.Programs, name)
		}
		if rs.Tier.Rank() > v.Tier.Rank() {
			v.Tier = rs.Tier
			v.Reason = rs.Reason
			v.Culprit = rs.Raw
			v.CulpritIndex = len(v.Segments)
		}
	}
	if len(v.Segments) == 0 {
		return RiskVerdict{Tier: RiskUnknown, Reason: "unreadable"}
	}
	if v.Reason == "" {
		v.Reason = reasonFor[v.Tier]
	}
	return v
}

// classifyRiskSegment reads one segment's argv.
func classifyRiskSegment(seg riskSegment, table map[string]CommandRiskRule) RiskSegment {
	out := RiskSegment{Raw: seg.raw}
	if seg.unreadable != "" {
		out.Tier, out.Reason = RiskUnknown, seg.unreadable
		return out
	}
	tokens := seg.tokens
	if len(tokens) == 0 {
		return out // Tier "" — the caller skips it
	}

	// Wrappers first, and bounded: a chain longer than this is not a
	// command anybody wrote by hand, and an unbounded loop over
	// attacker-shaped argv is not worth the elegance.
	floor := CommandRisk("")
	for range 4 {
		name := programName(tokens[0])
		if tokens[0].expands {
			out.Program, out.Tier, out.Reason = "", RiskUnknown, "variable_command"
			return out
		}
		w, isWrapper := wrapperCommands[name]
		if !isWrapper {
			break
		}
		rest, ok := unwrap(tokens[1:], w)
		if !ok {
			out.Program, out.Tier, out.Reason = name, RiskUnknown, "unreadable_wrapper"
			return out
		}
		if w.root {
			floor = floor.AtLeast(RiskDestructive)
			out.Via = name
		}
		tokens = rest
	}

	name := programName(tokens[0])
	if tokens[0].expands {
		out.Program, out.Tier, out.Reason = "", RiskUnknown, "variable_command"
		return out
	}
	out.Program = name

	if shellReservedWords[name] {
		// `for`, `while`, `if`, `time`. The body is not parsed; see the
		// non-goal in the design. Reported distinctly so the prompt can
		// say "shell loop" rather than "unrecognised command: for".
		out.Tier, out.Reason = RiskUnknown, "shell_keyword"
		return out
	}

	rule, found := table[name]
	if !found {
		// A classified network program still counts even when it is not
		// in the risk table — DefaultCommandClasses is the one place
		// "reaches off the box" is written down, and an operator who
		// adds a command there should not have to add it twice.
		if class, ok := ActiveCommandClasses()[name]; ok && class.Action != "" {
			out.Tier, out.Reason = riskOfAction(class.Action), "network_egress"
			return out.withFloor(floor)
		}
		out.Tier, out.Reason = RiskUnknown, "unrecognised_command"
		return out.withFloor(floor)
	}

	tier := rule.Tier
	if tier == "" {
		tier = RiskUnknown
	}
	args := tokens[1:]

	if len(rule.Sub) > 0 {
		sub, expands, ok := firstOperand(args)
		switch {
		case !ok:
			// Bare invocation: `git` on its own prints usage.
		case expands:
			out.Tier, out.Reason = RiskUnknown, "variable_subcommand"
			return out.withFloor(floor)
		default:
			if t, named := rule.Sub[sub]; named {
				tier = t
			} else {
				out.Tier, out.Reason = RiskUnknown, "unrecognised_subcommand"
				return out.withFloor(floor)
			}
		}
	} else if rule.OperandTier != "" {
		if _, _, ok := firstOperand(args); ok {
			tier = tier.AtLeast(rule.OperandTier)
		}
	}

	for _, tok := range args {
		for pattern, esc := range rule.Escalate {
			if escalateMatches(pattern, tok.text) {
				tier = tier.AtLeast(esc)
			}
		}
	}
	if len(seg.writeTargets) > 0 {
		// A redirection writes whatever the program on the left prints,
		// so the tier is at least a write however innocent that program
		// is. `echo pwned > ~/.ssh/authorized_keys` is the case that
		// makes this non-negotiable.
		tier = tier.AtLeast(RiskWrite)
	}
	reason := reasonFor[tier]

	// Only a targeting program's own operands are read as paths. For
	// anything else the operands are inputs — `grep root /etc/passwd`
	// reads a system file and changes nothing — so the only paths that
	// count are the ones being redirected into.
	var operands []riskToken
	if rule.Targets {
		operands = targetOperands(args, rule.TargetLast)
	}
	if len(operands) > 0 || len(seg.writeTargets) > 0 {
		tier, reason = applyTargets(tier, rule, operands, seg.writeTargets)
	}

	if rule.Reason != "" && tier == rule.Tier {
		reason = rule.Reason
	}
	out.Tier = tier
	out.Reason = reason
	return out.withFloor(floor)
}

// withFloor raises a segment to a floor a wrapper imposed, keeping the
// wrapper's reason where it is the thing that set the tier.
func (s RiskSegment) withFloor(floor CommandRisk) RiskSegment {
	if floor == "" || floor.Rank() <= s.Tier.Rank() {
		return s
	}
	s.Tier = floor
	s.Reason = "privilege_escalation"
	return s
}

// riskOfAction maps a command class's action onto a tier, so the two
// tables cannot drift.
func riskOfAction(action string) CommandRisk {
	switch action {
	case RemoteAction, RemoteCopyAction, NetFetchAction:
		return RiskNetwork
	default:
		return RiskUnknown
	}
}

// programName is the command as invoked, without its path:
// /usr/bin/ssh is ssh, the same reading ClassifyCommand takes.
func programName(t riskToken) string {
	name := t.text
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	return name
}

// targetOperands returns the non-flag arguments a targeting program
// acts on: all of them, or only the last when the rest are sources.
func targetOperands(args []riskToken, lastOnly bool) []riskToken {
	var operands []riskToken
	for _, a := range args {
		if strings.HasPrefix(a.text, "-") {
			continue
		}
		operands = append(operands, a)
	}
	if lastOnly && len(operands) > 1 {
		return operands[len(operands)-1:]
	}
	return operands
}

// firstOperand returns the first non-flag argument.
func firstOperand(args []riskToken) (text string, expands, ok bool) {
	for _, a := range args {
		if strings.HasPrefix(a.text, "-") {
			continue
		}
		return a.text, a.expands, true
	}
	return "", false, false
}

// unwrap drops a wrapper's own flags and operands, returning the
// command it runs. ok=false when there is nothing left, or when the
// wrapper carries something that changes what the command means.
func unwrap(args []riskToken, w wrapperSpec) ([]riskToken, bool) {
	skipped := 0
	for i := 0; i < len(args); i++ {
		tok := args[i].text
		if strings.HasPrefix(tok, "-") {
			if w.valueFlags[tok] {
				i++
			}
			continue
		}
		if w.refuseAssign && isEnvAssignment(tok) {
			return nil, false
		}
		if skipped < w.skipOperands {
			skipped++
			continue
		}
		return args[i:], true
	}
	return nil, false
}

// escalateMatches compares an Escalate key against a token: exact, or
// prefix when the key ends in "*".
func escalateMatches(pattern, tok string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(tok, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == tok
}

// ---------------------------------------------------------------------
// Reading the command line.
//
// Deliberately MORE permissive than NormaliseCommand, and the
// difference is the question being asked. That one asks "is there a
// stable name a grant could be written against", so a glob defeats it:
// `rm *` names a different set of files every time it runs. This one
// asks "what does it do", and a glob does not change the answer —
// `ls *.go` still reads and `rm *` still deletes. Refusing globs here
// would make the classifier blind to commands it can classify
// perfectly well.
//
// What it does refuse is anything that introduces a program it has not
// read: command substitution, backticks, a variable in the command
// slot, a subshell.

type riskToken struct {
	text string
	// expands records that the token carried a $ outside single
	// quotes, so its value is not what is written. Only consulted in
	// positions the classifier reads — argv[0] and a subcommand — since
	// elsewhere an expansion adds arguments to a program already
	// identified.
	expands bool
}

type riskSegment struct {
	raw    string
	tokens []riskToken
	// unreadable is a reason code, set when something in this segment
	// defeats static reading.
	unreadable string
	// writeTargets are the paths this segment redirects into, other
	// than /dev/null and file-descriptor dups. Kept as tokens rather
	// than a bool so `> /etc/passwd` and `> /tmp/probe` can be told
	// apart.
	writeTargets []riskToken
}

// splitRiskSegments breaks a command line into the commands it runs.
//
// ok=false only for input that is not a command line at all: an
// unterminated quote, or a control character. Everything else produces
// segments, some of which may be marked unreadable.
func splitRiskSegments(cmd string) ([]riskSegment, bool) {
	sc := &riskScanner{runes: []rune(cmd)}
	if !sc.scan() {
		return nil, false
	}
	return sc.segs, true
}

// riskScanner walks a command line once, accumulating tokens into
// segments.
//
// A struct rather than a closure over locals, because the dispatch is
// wide — quotes, substitutions, redirects, separators — and each kind
// needs the same five accumulators. Sharing them through a receiver is
// what lets the loop body split into methods that are each readable on
// their own.
type riskScanner struct {
	runes []rune
	segs  []riskSegment
	cur   riskSegment

	tok     strings.Builder
	started bool
	expands bool

	segStart int
	// subDepth counts open "$(" and backtick reports an open backtick.
	// Inside either, the separators belong to a command this scanner is
	// not reading, so they must not split anything out here.
	subDepth int
	backtick bool
}

// scan runs the walk. false means the input is not a command line.
func (s *riskScanner) scan() bool {
	for i := 0; i < len(s.runes); i++ {
		r := s.runes[i]
		if !riskReadableRune(r) {
			return false
		}
		if r == '\'' || r == '"' {
			q, ok := scanQuoted(s.runes, i)
			if !ok {
				return false
			}
			s.addQuoted(q)
			i = q.end
			continue
		}
		// Order is load-bearing. Substitution and redirect handling run
		// BEFORE the inside-a-substitution check, so `$(` still opens a
		// depth and `)` still closes one; everything after it is inert
		// while a substitution is open.
		if next, handled := s.scanUnreadable(r, i); handled {
			i = next
			continue
		}
		if next, handled := s.scanRedirect(r, i); handled {
			i = next
			continue
		}
		if s.subDepth > 0 || s.backtick {
			s.write(r)
			continue
		}
		if next, handled := s.scanSeparator(r, i); handled {
			i = next
			continue
		}
		s.write(r)
	}
	s.flushSegment(len(s.runes))
	return true
}

// scanUnreadable handles the runes that mean this segment cannot be
// read statically: an escape, a substitution, a subshell.
func (s *riskScanner) scanUnreadable(r rune, i int) (int, bool) {
	switch r {
	case '\\':
		// An escape changes what the next character means in ways the
		// token text would not preserve.
		s.cur.unreadable = "escaped_command"
		s.started = true
	case '`':
		s.cur.unreadable = "command_substitution"
		s.backtick = !s.backtick
		s.started = true
	case '$':
		if i+1 < len(s.runes) && s.runes[i+1] == '(' {
			s.cur.unreadable = "command_substitution"
			s.subDepth++
			s.started = true
			return i + 1, true
		}
		s.expands = true
		s.tok.WriteRune(r)
		s.started = true
	case '(', ')':
		if r == ')' && s.subDepth > 0 {
			s.subDepth--
			return i, true
		}
		// A subshell runs its contents somewhere this classifier is not
		// reading.
		s.cur.unreadable = "subshell"
		s.started = true
	default:
		return i, false
	}
	return i, true
}

// scanRedirect handles ">", "<" and bash's "&>".
func (s *riskScanner) scanRedirect(r rune, i int) (int, bool) {
	at := i
	switch {
	case r == '>' || r == '<':
		// A bare number in front of a redirect is a file descriptor,
		// not an argument: `apt-get --version 2>&1` passes no operand
		// called "2", and treating it as one would send the subcommand
		// lookup hunting for it.
		if s.started && allDigits(s.tok.String()) {
			s.tok.Reset()
			s.started, s.expands = false, false
		}
	case r == '&' && i+1 < len(s.runes) && s.runes[i+1] == '>':
		at = i + 1 // "&>file": stdout and stderr to one place
	default:
		return i, false
	}
	s.flushToken()
	return consumeRedirect(s.runes, at, &s.cur), true
}

// scanSeparator handles whitespace and the operators that end a
// command: newline, ";", "&", "|", "&&", "||".
func (s *riskScanner) scanSeparator(r rune, i int) (int, bool) {
	switch r {
	case ' ', '\t':
		s.flushToken()
		return i, true
	case '\n', ';':
		s.flushSegment(i)
		s.segStart = i + 1
		return i, true
	case '&', '|':
		end, next := i, i
		if i+1 < len(s.runes) && s.runes[i+1] == r {
			next++ // "&&" or "||"
		}
		s.flushSegment(end)
		s.segStart = next + 1
		return next, true
	}
	return i, false
}

// addQuoted folds a quoted run into the token being built.
func (s *riskScanner) addQuoted(q quotedSpan) {
	if q.substitutes {
		s.cur.unreadable = "command_substitution"
	}
	if q.expands {
		s.expands = true
	}
	s.tok.WriteString(q.text)
	s.started = true
}

// write appends an ordinary rune to the token being built.
func (s *riskScanner) write(r rune) {
	s.tok.WriteRune(r)
	s.started = true
}

func (s *riskScanner) flushToken() {
	if s.started {
		s.cur.tokens = append(s.cur.tokens, riskToken{text: s.tok.String(), expands: s.expands})
		s.tok.Reset()
		s.started, s.expands = false, false
	}
}

func (s *riskScanner) flushSegment(end int) {
	s.flushToken()
	s.cur.raw = strings.TrimSpace(string(s.runes[s.segStart:end]))
	if s.cur.raw != "" || len(s.cur.tokens) > 0 {
		s.segs = append(s.segs, s.cur)
	}
	s.cur = riskSegment{}
}

// riskReadableRune rejects the runes that make one string display as
// another.
//
// A segment that DISPLAYS as one command and IS another is consent
// obtained by misdirection, and the prompt quotes this text back at the
// user. Same refusal NormaliseCommand makes, for the same reason.
func riskReadableRune(r rune) bool {
	if unicode.IsControl(r) && r != '\n' && r != '\t' {
		return false
	}
	return !isInvisible(r) && !(r > unicode.MaxASCII && unicode.IsSpace(r))
}

// quotedSpan is what a quoted run contributes to the token being built.
type quotedSpan struct {
	text string
	// expands records a $ that is still live inside the quotes.
	expands bool
	// substitutes records a $( or a backtick, which runs a program
	// this classifier has not read.
	substitutes bool
	// end is the index of the closing quote.
	end int
}

// scanQuoted reads the quoted run starting at runes[i].
//
// Single quotes are literal; double quotes are not, and that
// difference is the whole reason this is separate. Expansion and
// substitution both still happen inside double quotes, so quoting does
// NOT make `"$(rm -rf /)"` inert — a scanner that treated every quoted
// span as data would classify it as an ordinary argument.
//
// ok=false for an unterminated quote, which is not a command line.
func scanQuoted(runes []rune, i int) (quotedSpan, bool) {
	quote := runes[i]
	closing := indexRune(runes, i+1, quote)
	if closing < 0 {
		return quotedSpan{}, false
	}
	inner := runes[i+1 : closing]
	span := quotedSpan{text: string(inner), end: closing}
	if quote == '\'' {
		return span, true
	}
	for j := 0; j < len(inner); j++ {
		switch inner[j] {
		case '`':
			span.substitutes = true
		case '$':
			if j+1 < len(inner) && inner[j+1] == '(' {
				span.substitutes = true
			} else {
				span.expands = true
			}
		}
	}
	return span, true
}

// consumeRedirect reads a redirection starting at the ">" or "<" in
// runes[i], recording on seg whether it writes somewhere real, and
// returns the index of the last rune consumed.
func consumeRedirect(runes []rune, i int, seg *riskSegment) int {
	reading := runes[i] == '<'
	j := i + 1
	if j < len(runes) && (runes[j] == '>' || runes[j] == '<') {
		j++ // ">>" append, "<<" heredoc
	}
	for j < len(runes) && (runes[j] == ' ' || runes[j] == '\t') {
		j++
	}
	start := j
	// A leading "&" is a file-descriptor dup ("2>&1"), not the
	// backgrounding operator, so it belongs to the target rather than
	// ending it.
	if j < len(runes) && runes[j] == '&' {
		j++
	}
	for j < len(runes) && !strings.ContainsRune(" \t\n;|&()<>", runes[j]) {
		j++
	}
	target := string(runes[start:j])
	switch {
	case reading:
		// Reading from a file changes nothing.
	case strings.HasPrefix(target, "&"):
		// A file-descriptor dup: `2>&1` writes nowhere new.
	case target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr":
		// The conventional way to say "discard", and the reason a
		// probe full of `2>/dev/null` is not reported as a write.
	case target == "":
		seg.unreadable = "unreadable_redirect"
	default:
		seg.writeTargets = append(seg.writeTargets, riskToken{
			text:    target,
			expands: strings.Contains(target, "$"),
		})
	}
	return j - 1
}

// allDigits reports whether s is a bare number, which in front of a
// redirect means a file descriptor.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// indexRune finds r at or after start, or -1.
func indexRune(runes []rune, start int, r rune) int {
	for i := start; i < len(runes); i++ {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------
// The operator's table, and the tier on the request context.

// activeCommandRisks is the table in force, in the shape
// activeCommandClasses already uses.
var activeCommandRisks atomic.Pointer[map[string]CommandRiskRule]

// SetCommandRisks installs the operator's table, MERGED over the
// shipped one rather than replacing it.
//
// The opposite of SetCommandClasses, deliberately. That table has six
// entries and an operator can reasonably restate it; this one has
// hundreds, and replacing it wholesale would mean adding one in-house
// tool silently reclassified every command in the table as unknown —
// turning a small config edit into a flood of confirmations, which is
// the failure this whole change exists to remove. An entry with an
// empty tier removes the shipped one, which is how somebody says "stop
// classifying this".
func SetCommandRisks(m map[string]CommandRiskRule) {
	if len(m) == 0 {
		activeCommandRisks.Store(nil)
		return
	}
	merged := make(map[string]CommandRiskRule, len(DefaultCommandRisks)+len(m))
	for k, v := range DefaultCommandRisks {
		merged[k] = v
	}
	for k, v := range m {
		if v.Tier == "" && len(v.Sub) == 0 && len(v.Escalate) == 0 && v.OperandTier == "" {
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	activeCommandRisks.Store(&merged)
}

// ActiveCommandRisks returns the table in force.
func ActiveCommandRisks() map[string]CommandRiskRule {
	if m := activeCommandRisks.Load(); m != nil {
		return *m
	}
	return DefaultCommandRisks
}

// riskProgramsShown bounds the program list in a headline. Past this
// the list stops being a summary and becomes another wall.
const riskProgramsShown = 8

// RiskHeadline is the one line that leads a confirmation prompt.
//
// It answers, in order, the three things somebody about to tap Approve
// needs: what KIND of thing this is, WHICH part made it that, and what
// it touches. The verbatim command still follows — this is added to
// it, never instead of it.
func RiskHeadline(v RiskVerdict) string {
	if v.Tier == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(v.Tier.Label())

	switch {
	case v.Tier == RiskUnknown && v.Unreadable > 0 && len(v.Segments) > 1:
		fmt.Fprintf(&b, " · %d of %d steps unreadable (%s)",
			v.Unreadable, len(v.Segments), v.Reason)
	case v.Tier != RiskRead && v.CulpritIndex > 0 && len(v.Segments) > 1:
		// Naming the step is the largest readability win there is: in a
		// 300-character probe, one `rm` is why the question is being
		// asked and the other eight steps are noise.
		fmt.Fprintf(&b, " · `%s` (step %d of %d)", v.Culprit, v.CulpritIndex, len(v.Segments))
	case v.Reason != "":
		b.WriteString(" · " + v.Reason)
	}

	if len(v.Programs) > 0 {
		b.WriteString(" · ")
		if len(v.Programs) > riskProgramsShown {
			b.WriteString(strings.Join(v.Programs[:riskProgramsShown], ", "))
			fmt.Fprintf(&b, " +%d more", len(v.Programs)-riskProgramsShown)
		} else {
			b.WriteString(strings.Join(v.Programs, ", "))
		}
	}
	if v.FromModel {
		// Said out loud, because a tier a model moved is a different
		// kind of claim from one the classifier read off the argv.
		b.WriteString(" · model")
	}
	return b.String()
}

// RiskGrantResource is the key a grant covering a whole TIER is
// recorded under.
//
// A sentinel in the shape of the "(cwd=…)" and "(remote=…)" keys and
// of !unclassified: a real key always begins with a rendered command
// token, and NormaliseCommand single-quotes anything starting with "("
// — so no command can land in this namespace by accident, and an
// operator writing one has said what they meant.
func RiskGrantResource(tier CommandRisk) string {
	if !tier.Valid() {
		return ""
	}
	return "(risk=" + string(tier) + ")"
}

// commandRiskKey carries the classified tier from the approval gate to
// the policy condition evaluator.
//
// On the context rather than in the Evaluate signature, because the
// engine's question is (subject, action, resource) and widening it for
// one condition would put a shell concept into every policy check.
// ConditionEvaluator already takes a ctx for exactly this.
type commandRiskKey struct{}

// WithCommandRisk records the tier this request was classified into.
//
// The tier comes from the classifier over the parameters the executor
// is about to run, never from anything the model wrote as prose — the
// same reason the turn identity comes from the request context.
func WithCommandRisk(ctx context.Context, tier CommandRisk) context.Context {
	if !tier.Valid() {
		return ctx
	}
	return context.WithValue(ctx, commandRiskKey{}, tier)
}

// CommandRiskFrom reads the tier back. ok=false means this request was
// never classified — a memory write, say — and a rule conditioned on a
// tier must not apply to it.
func CommandRiskFrom(ctx context.Context) (CommandRisk, bool) {
	t, ok := ctx.Value(commandRiskKey{}).(CommandRisk)
	return t, ok
}
