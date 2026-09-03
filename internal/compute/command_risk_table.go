package compute

import "strings"

// The catalogue: what each program DOES.
//
// Data, kept apart from the engine that reads it. The classifier in
// command_risk.go is parsing and set arithmetic that changes rarely;
// this is a list that grows every time somebody meets a program it has
// not heard of, and reviewing a new entry should not mean reading a
// diff against the shell tokeniser.
//
// EXTENDING IT DOES NOT REQUIRE EDITING THIS FILE. [compute.command_risks]
// merges over the table at startup, so a deployment adds or overrides
// entries in config:
//
//	[compute.command_risks]
//	terraform = { labels = ["reads"], subcommands = { destroy = ["deletes", "disrupts"] } }
//
// A pull request here is for entries everyone should have; config is
// for the ones only you have. What follows is the shipped default that
// config merges onto.
//
// Two rules govern every entry, and both are about being wrong safely:
//
//   - A program that is not here is UNREADABLE, so an incomplete table
//     asks rather than waves through. Entries are added when somebody
//     has thought about what the program does with every flag it might
//     carry, not to make the list look finished.
//   - A program whose verb is a subcommand (Sub) or a flag (FlagSub)
//     and whose verb is not listed is unreadable too — never the base
//     Labels. `pacman -Rdd` must not read as "reads" because nobody
//     enumerated -Rdd.

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
	"apk": packageRisk,
	// Arch, and the AUR helpers that wrap it.
	"pacman": pacmanRisk, "paru": aurRisk, "yay": aurRisk, "pikaur": aurRisk,
	"pamac": aurRisk, "trizen": aurRisk, "aurutils": aurRisk,
	// dpkg and rpm take their verb as a flag, so they use FlagSub: a
	// spelling nobody enumerated is unreadable rather than inheriting
	// "reads". They were Escalate before, which is additive — so
	// `dpkg --audit` or any unlisted flag read as harmless.
	"dpkg": {Labels: L(LabelReads), FlagSub: map[string][]RiskLabel{
		"-l": L(LabelReads), "--list": L(LabelReads),
		"-L": L(LabelReads), "--listfiles": L(LabelReads),
		"-S": L(LabelReads), "--search": L(LabelReads),
		"-s": L(LabelReads), "--status": L(LabelReads),
		"-p": L(LabelReads), "-I": L(LabelReads), "-c": L(LabelReads),
		"--audit": L(LabelReads), "--verify": L(LabelReads), "--print-architecture": L(LabelReads),
		// Installing a .deb runs its maintainer scripts as root.
		"-i": L(LabelPrivilege, LabelWrites), "--install": L(LabelPrivilege, LabelWrites),
		"--unpack":    L(LabelPrivilege, LabelWrites),
		"--configure": L(LabelPrivilege, LabelWrites),
		"-r":          L(LabelDeletes, LabelPrivilege), "--remove": L(LabelDeletes, LabelPrivilege),
		"-P": L(LabelDeletes, LabelPrivilege), "--purge": L(LabelDeletes, LabelPrivilege),
	}},
	"dpkg-query":       {Labels: L(LabelReads)},
	"dpkg-reconfigure": {Labels: L(LabelPrivilege, LabelWrites)},
	"rpm": {Labels: L(LabelReads), FlagSub: map[string][]RiskLabel{
		"-q": L(LabelReads), "-qa": L(LabelReads), "-qi": L(LabelReads),
		"-ql": L(LabelReads), "-qf": L(LabelReads), "-qp": L(LabelReads),
		"--query": L(LabelReads), "-V": L(LabelReads), "--verify": L(LabelReads),
		"-K": L(LabelReads), "--checksig": L(LabelReads),
		// %post scriptlets, as root.
		"-i": L(LabelPrivilege, LabelWrites), "--install": L(LabelPrivilege, LabelWrites),
		"-U": L(LabelPrivilege, LabelWrites), "--upgrade": L(LabelPrivilege, LabelWrites),
		"-F": L(LabelPrivilege, LabelWrites), "--freshen": L(LabelPrivilege, LabelWrites),
		"-ivh": L(LabelPrivilege, LabelWrites), "-Uvh": L(LabelPrivilege, LabelWrites),
		"-e": L(LabelDeletes, LabelPrivilege), "--erase": L(LabelDeletes, LabelPrivilege),
	}},
	"rpm-ostree": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"status": L(LabelReads), "db": L(LabelReads),
		"install":   L(LabelNetwork, LabelPrivilege, LabelWrites),
		"upgrade":   L(LabelNetwork, LabelPrivilege, LabelWrites),
		"uninstall": L(LabelDeletes, LabelPrivilege),
		"rollback":  L(LabelPrivilege, LabelDisrupts),
	}},

	// Source-building and user-scope managers. The distinction that
	// matters is PRIVILEGE: emerge and snap need root, while brew,
	// cargo, nix, go and pipx install into a user prefix and do not.
	// Five models were polled and were consistent about which is which.
	"emerge": {Labels: L(LabelNetwork, LabelPrivilege, LabelWrites), Escalate: map[string][]RiskLabel{
		"--depclean": L(LabelDeletes), "-c": L(LabelDeletes),
		"--unmerge": L(LabelDeletes), "-C": L(LabelDeletes),
		"--search": L(LabelReads), "-s": L(LabelReads),
	}},
	"xbps-install": {Labels: L(LabelNetwork, LabelPrivilege, LabelWrites)},
	"xbps-remove":  {Labels: L(LabelDeletes, LabelPrivilege)},
	"xbps-query":   {Labels: L(LabelReads)},
	"snap": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"list": L(LabelReads), "find": L(LabelNetwork, LabelReads), "info": L(LabelReads),
		"install": L(LabelNetwork, LabelPrivilege, LabelWrites),
		"refresh": L(LabelNetwork, LabelPrivilege, LabelWrites),
		"remove":  L(LabelDeletes, LabelPrivilege),
		"disable": L(LabelDisrupts, LabelPrivilege), "enable": L(LabelDisrupts, LabelPrivilege),
	}},
	"flatpak": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"list": L(LabelReads), "info": L(LabelReads), "search": L(LabelNetwork, LabelReads),
		"install": L(LabelNetwork, LabelWrites), "update": L(LabelNetwork, LabelWrites),
		"uninstall": L(LabelDeletes), "remote-add": L(LabelNetwork, LabelWrites),
	}},
	"brew": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"list": L(LabelReads), "info": L(LabelReads), "search": L(LabelNetwork, LabelReads),
		"doctor": L(LabelReads), "outdated": L(LabelNetwork, LabelReads),
		"install": L(LabelNetwork, LabelWrites), "upgrade": L(LabelNetwork, LabelWrites),
		"update": L(LabelNetwork, LabelWrites), "tap": L(LabelNetwork, LabelWrites),
		"uninstall": L(LabelDeletes), "remove": L(LabelDeletes), "cleanup": L(LabelDeletes),
	}},
	"nix-env":   {Labels: L(LabelNetwork, LabelWrites)},
	"nix-shell": {Labels: L(LabelUnreadable)},
	"nix": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"search": L(LabelNetwork, LabelReads), "show": L(LabelReads),
		"build": L(LabelNetwork, LabelWrites), "profile": L(LabelNetwork, LabelWrites),
		"develop": L(LabelUnreadable), "run": L(LabelUnreadable), "shell": L(LabelUnreadable),
	}},
	"cargo": {Labels: L(LabelUnreadable), Sub: map[string][]RiskLabel{
		"tree": L(LabelReads), "search": L(LabelNetwork, LabelReads),
		"fetch": L(LabelNetwork, LabelWrites), "install": L(LabelNetwork, LabelWrites),
		"uninstall": L(LabelDeletes), "clean": L(LabelDeletes),
	}},
	"gem": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"list": L(LabelReads), "search": L(LabelNetwork, LabelReads),
		"install": L(LabelNetwork, LabelWrites), "update": L(LabelNetwork, LabelWrites),
		"uninstall": L(LabelDeletes),
	}},
	"pipx": {Labels: L(LabelReads), Sub: map[string][]RiskLabel{
		"list":    L(LabelReads),
		"install": L(LabelNetwork, LabelWrites), "upgrade": L(LabelNetwork, LabelWrites),
		"uninstall": L(LabelDeletes), "runpip": L(LabelNetwork, LabelWrites),
		"run": L(LabelUnreadable),
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
	"make": runsCode, "cmake": runsCode, "go": runsCode,
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

// packageRisk covers the SUBCOMMAND-driven distro package managers:
// apt, dnf, zypper, apk.
//
// Every mutating verb carries privilege, which this table missed until
// five models were polled about it and all five said so. They are
// right: installing or removing a system package needs root, and a
// classification that called `apt-get install` merely "network" was
// describing the smaller half of what happens.
//
// Deliberately NOT unreadable, though several models reached for it —
// a package install does run maintainer scripts as root, which is
// genuinely code nobody has read. But unreadable means "the classifier
// could not read this COMMAND", it is approvable by no configuration,
// and resolve_unknown treats a bare unreadable as the gap a model may
// fill. Overloading it with "runs third-party code" would give one
// label two meanings, which is the fault the label set exists to fix.
// network + privilege already says it: fetching from a remote and
// running as root IS arbitrary remote code as root.
var packageRisk = CommandRiskRule{Labels: L(LabelReads), Sub: map[string][]RiskLabel{
	"list": L(LabelReads), "show": L(LabelReads), "search": L(LabelReads),
	"policy": L(LabelReads), "info": L(LabelReads), "depends": L(LabelReads),
	"why": L(LabelReads), "provides": L(LabelReads),

	"update":       L(LabelNetwork, LabelPrivilege, LabelWrites),
	"install":      L(LabelNetwork, LabelPrivilege, LabelWrites),
	"add":          L(LabelNetwork, LabelPrivilege, LabelWrites),
	"reinstall":    L(LabelNetwork, LabelPrivilege, LabelWrites),
	"upgrade":      L(LabelNetwork, LabelPrivilege, LabelWrites),
	"dist-upgrade": L(LabelNetwork, LabelPrivilege, LabelWrites),
	"full-upgrade": L(LabelNetwork, LabelPrivilege, LabelWrites),
	// A download writes to the cache and needs no root.
	"download": L(LabelNetwork, LabelWrites),
	"source":   L(LabelNetwork, LabelWrites),

	"remove":     L(LabelDeletes, LabelPrivilege),
	"del":        L(LabelDeletes, LabelPrivilege),
	"purge":      L(LabelDeletes, LabelPrivilege),
	"autoremove": L(LabelDeletes, LabelPrivilege),
	"erase":      L(LabelDeletes, LabelPrivilege),
	"clean":      L(LabelDeletes, LabelPrivilege),
}}

// pacmanRisk is the flag-driven Arch family.
//
// FlagSub rather than Escalate, so an operation nobody enumerated is
// unreadable instead of inheriting the base "reads". `pacman -Rdd foo`
// removes a package and ignores its dependencies; there are more flag
// combinations than anybody will list, and the unlisted ones must not
// read as harmless.
var pacmanRisk = CommandRiskRule{Labels: L(LabelReads), FlagSub: map[string][]RiskLabel{
	// Query: local database, no root, no network.
	"-Q": L(LabelReads), "-Qi": L(LabelReads), "-Ql": L(LabelReads),
	"-Qo": L(LabelReads), "-Qe": L(LabelReads), "-Qm": L(LabelReads),
	"-Qn": L(LabelReads), "-Qs": L(LabelReads), "-Qdt": L(LabelReads),
	"-Qu": L(LabelReads), "--query": L(LabelReads),
	"-T": L(LabelReads), "-F": L(LabelReads), "-Fl": L(LabelReads),

	// Sync: searching is a read of a synced database; anything that
	// installs needs root and reaches a mirror.
	"-Ss": L(LabelReads), "-Si": L(LabelReads), "-Sg": L(LabelReads),
	"-Sl": L(LabelReads), "-Sp": L(LabelReads),
	"-S":     L(LabelNetwork, LabelPrivilege, LabelWrites),
	"-Sy":    L(LabelNetwork, LabelPrivilege, LabelWrites),
	"-Su":    L(LabelNetwork, LabelPrivilege, LabelWrites),
	"-Syu":   L(LabelNetwork, LabelPrivilege, LabelWrites),
	"-Syyu":  L(LabelNetwork, LabelPrivilege, LabelWrites),
	"-Syyuu": L(LabelNetwork, LabelPrivilege, LabelWrites),
	"-Sw":    L(LabelNetwork, LabelWrites),
	"--sync": L(LabelNetwork, LabelPrivilege, LabelWrites),
	// Cache cleaning throws packages away.
	"-Sc": L(LabelDeletes, LabelPrivilege), "-Scc": L(LabelDeletes, LabelPrivilege),

	// Remove, in its several depth-of-destruction spellings.
	"-R": L(LabelDeletes, LabelPrivilege), "-Rs": L(LabelDeletes, LabelPrivilege),
	"-Rn": L(LabelDeletes, LabelPrivilege), "-Rns": L(LabelDeletes, LabelPrivilege),
	"-Rsc": L(LabelDeletes, LabelPrivilege), "-Rdd": L(LabelDeletes, LabelPrivilege),
	"-Rcns": L(LabelDeletes, LabelPrivilege), "--remove": L(LabelDeletes, LabelPrivilege),

	// A local package file, installed with its scriptlets, as root.
	"-U": L(LabelPrivilege, LabelWrites), "--upgrade": L(LabelPrivilege, LabelWrites),
	"-D": L(LabelPrivilege, LabelWrites), "--database": L(LabelPrivilege, LabelWrites),
}}

// aurRisk wraps pacman for the AUR helpers.
//
// Same operations, plus a network hop for anything that touches the
// AUR — including a search, which pacman does locally and paru does
// over the wire.
var aurRisk = CommandRiskRule{Labels: L(LabelReads), FlagSub: aurFlags()}

// aurFlags is pacmanRisk's table with the searches given a network
// label, built rather than restated so the two cannot drift.
func aurFlags() map[string][]RiskLabel {
	out := make(map[string][]RiskLabel, len(pacmanRisk.FlagSub))
	for flag, labels := range pacmanRisk.FlagSub {
		if len(labels) == 1 && labels[0] == LabelReads && strings.HasPrefix(flag, "-S") {
			out[flag] = L(LabelReads, LabelNetwork)
			continue
		}
		out[flag] = labels
	}
	return out
}

// pipRisk is the python package manager// pipRisk is the python package manager, whose install reaches out and
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
	"env":     {refuseAssign: true, bareLabels: L(LabelReads)},
	"setsid":  {},
}
