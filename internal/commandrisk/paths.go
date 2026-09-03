package commandrisk

import (
	"path"
	"strings"
	"sync/atomic"
)

// What a command is POINTED AT, which decides as much as its name does.
//
// `rm /tmp/probe`, `rm -rf /` and `rm -rf $DIR` are one program and
// three different operations.

// pathScope is what a target path is part of.
type pathScope int

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
			return MergeLabels(labels, L(LabelPrivilege)), "system_path"
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
