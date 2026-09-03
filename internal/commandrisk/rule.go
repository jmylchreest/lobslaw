package commandrisk

import "strings"

// The shape of a catalogue entry, and the wrappers that run other
// programs.

// CommandRiskRule is how one program name is classified.
//
// Shaped like CommandClass in command_class.go: a compiled table an
// operator extends through config rather than a heuristic. The extra
// fields exist because a program name alone is not always enough —
// `git status` and `git clean -fdx` are the same program.
type CommandRiskRule struct {
	// Extends names another entry to inherit from, so a family that
	// behaves identically is stated once.
	//
	// apt, dnf, zypper and apk are the same verbs with different names
	// on the front; paru and yay are pacman plus a network hop. Writing
	// each out in full is how two of them eventually disagree about
	// what "remove" does. A child's own fields override the parent's,
	// key by key, so extending and then correcting one verb is a
	// two-line entry.
	Extends string

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

	// FlagSub classifies by the first FLAG, for programs whose verbs are
	// flags rather than words: `pacman -S`, `rpm -e`, `dpkg -i`.
	//
	// The flag equivalent of Sub, with the same fail-closed rule and for
	// the same reason: a flag nobody named is UNREADABLE rather than
	// taking Labels. `pacman -Rdd foo` removes a package ignoring its
	// dependencies, and a table that has not heard of `-Rdd` must not
	// answer "reads" because the base entry happens to say so. There
	// are more pacman flag combinations than anybody will enumerate, so
	// the unenumerated case has to be the safe one.
	FlagSub map[string][]RiskLabel

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

	// bareLabels is what the wrapper does on its own, when it wraps
	// nothing. `env` with no command prints the environment, which is
	// a read; treating that as unreadable made a common probe ask for
	// no reason. Nil means a bare invocation is still unknown.
	bareLabels []RiskLabel
}

// wrapsSomething reports whether the wrapper was given anything at all
// beyond its own flags — as opposed to being invoked bare.
func (w wrapperSpec) wrapsSomething(args []riskToken) bool {
	for _, a := range args {
		if !strings.HasPrefix(a.text, "-") {
			return true
		}
	}
	return false
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
	"env":     {refuseAssign: true, bareLabels: L(LabelReads)},
	"setsid":  {},
}
