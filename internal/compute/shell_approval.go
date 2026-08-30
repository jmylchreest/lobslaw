package compute

import (
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Asking about the command instead of about the tool.
//
// Policy asks one question per tool name, so the only shell approval
// available was "allow shell_command" — every shell command, forever.
// Nobody should tap that, which meant "always" was unusable and the
// operator was left editing config. Worse, the shape underneath was a
// substring denylist inside the builtin: `ssh`, `curl`, `sudo` and ten
// others were refused outright, with no approval path at all, so the
// answer to "let me run this one ssh" was to go and edit a Go file.
//
// The gate below asks about the command. It is a POLICY question
// rather than a branch inside the tool, for the same reason the memory
// write gate is: it makes the answer reusable. A session grant covers
// the conversation, an "always" mints a revocable rule naming that one
// command, and an operator who wants a class writes an ordinary rule
// (resource = "git *") that outranks the default.
//
// The denylist is gone rather than promoted into a promptable list.
// The default rule asks about everything, which subsumes it. Keeping a
// code-level escalation on top would have meant an exact grant on
// `sudo systemctl restart nginx` could never take effect, which is the
// original complaint wearing a new hat. The hardline floor is
// untouched and stays the only unpromptable layer.

// ShellAction is the policy action a shell command is checked under.
//
// NOT tool:exec. wire_seeds.go seeds allow tool:exec/shell_command at
// priority 1 on every node, so a gate asking under tool:exec would be
// satisfied before it was asked. NOT command:exec either — that is the
// slash-command namespace (gateway.CommandAction), and two different
// questions sharing an action means one operator rule silently answers
// both.
const ShellAction = "shell:run"

// UnclassifiedResource is the resource a command with no derivable key is
// evaluated under.
//
// A real key always starts with a rendered token, and a token
// beginning with "!" renders single-quoted, so no command can reach
// this namespace by accident. An operator who writes an allow for it
// has explicitly said "stop asking me about compound commands".
const UnclassifiedResource = "!unclassified"

// ShellApprovalDefault is the rule that makes the gate ask.
//
// A rule rather than a hardcoded branch, so it composes: an operator
// overrides it with anything of higher priority, an approval mints an
// allow that outranks it, and it appears wherever rules appear rather
// than being invisible behaviour.
//
// Priority is the lowest the type allows. A default that could outrank
// an operator's rule would not be a default.
func ShellApprovalDefault() types.PolicyRule {
	return types.PolicyRule{
		ID:       "config:shell.approval",
		Subject:  "*",
		Action:   ShellAction,
		Resource: "*",
		Effect:   types.EffectRequireConfirmation,
		Priority: -1 << 30,
	}
}

// ShellGrantResource derives the resource a shell call is evaluated
// and granted under.
//
// grantable=false means the call is still confirmable but nothing may
// be minted from it: the channel offers Approve and Deny only. The
// resource is still returned in that case, because policy must be able
// to match on it even when no approval can create a rule for it.
func ShellGrantResource(params map[string]string) GrantTarget {
	raw := params["command"]
	key, ok := NormaliseCommand(raw)
	if !ok {
		return GrantTarget{Action: ShellAction, Resource: UnclassifiedResource}
	}

	// A command that reaches off the box is governed by what it does,
	// not by the tool that ran it. `ssh web01 git pull` here and
	// remote_ssh(remote=web01, command="git pull") are the same
	// operation, so they resolve to the same action and the same key
	// and one rule covers both. Without this, an operator who gated
	// remote_ssh would have said nothing about the shell reaching the
	// very same host.
	if tokens, tok := shellTokens(raw); tok {
		if class, host, rest, classified := ClassifyCommand(ActiveCommandClasses(), tokens); classified {
			if host == "" {
				// Classified but the target could not be read out of
				// the argv. Confirmable, never grantable: a standing
				// grant naming a host we guessed at is worse than
				// asking again.
				return GrantTarget{Action: class.Action, Resource: UnclassifiedResource}
			}
			remoteKey, rok := NormaliseCommand(strings.Join(rest, " "))
			if !rok || len(rest) == 0 {
				// scp and friends have no remote command, and an ssh
				// with none opens a shell. Either way the key is the
				// host itself.
				remoteKey = ""
			}
			return remoteTarget(class.Action, host, remoteKey)
		}
	}
	if cwd := strings.TrimSpace(params["cwd"]); cwd != "" {
		// Only when the caller supplied one. The model usually omits
		// cwd, so most keys stay clean — but `git clean -fd` in two
		// different checkouts is two different operations, and an
		// approval given for one must not cover the other.
		if !cwdUsableInKey(cwd) {
			return GrantTarget{Action: ShellAction, Resource: UnclassifiedResource}
		}
		key = "(cwd=" + cwd + ") " + key
	}
	if len([]rune(key)) > shellKeyDisplayMax {
		// Matchable, not grantable. See shellKeyDisplayMax.
		return GrantTarget{Action: ShellAction, Resource: key}
	}
	if strings.Contains(key, "*") {
		// ApprovalRules.Mint refuses any resource containing a
		// wildcard, so offering "always" here would render a button
		// that reports success and stores nothing. A quoted glob —
		// `grep '*.go'` — is a literal to the shell but still a
		// wildcard to the engine's matcher.
		return GrantTarget{Action: ShellAction, Resource: key}
	}
	return GrantTarget{Action: ShellAction, Resource: key, Grantable: true}
}

// remoteTarget builds the grant for a host-aimed command, applying the
// same grantability rules the shell key gets: too long to display, or
// carrying a wildcard the minter would refuse, means confirmable but
// not mintable.
func remoteTarget(action, host, command string) GrantTarget {
	key := RemoteResourceKey(host, command)
	if len([]rune(key)) > shellKeyDisplayMax || strings.Contains(key, "*") {
		return GrantTarget{Action: action, Resource: key}
	}
	return GrantTarget{Action: action, Resource: key, Grantable: true}
}

// ShellCommandSummary renders the call for the confirmation prompt.
//
// The command appears verbatim and in full. A prompt that paraphrases
// what is about to run cannot be answered, and one that truncates it
// invites approving the part that was not shown.
//
// When the call is grantable the summary contains the resource
// byte-for-byte, so what the user reads is what gets minted. That is
// the property the whole design rests on and it is asserted in the
// tests.
func ShellCommandSummary(params map[string]string) string {
	cmd := strings.TrimSpace(params["command"])
	if cmd == "" {
		return ""
	}
	t := ShellGrantResource(params)
	resource, grantable := t.Resource, t.Grantable

	var b strings.Builder
	b.WriteString("run `")
	if grantable {
		b.WriteString(resource)
	} else {
		// Nothing will be minted, so there is no key to agree with —
		// but the user still has to see exactly what runs.
		if cwd := strings.TrimSpace(params["cwd"]); cwd != "" {
			// Only when it is safe to render. cwdUsableInKey rejects
			// the runes that make one string display as another, and
			// this is the branch those calls land in — printing the raw
			// value here would put a bidi override into the prompt and
			// undo the check that rejected it.
			if cwdUsableInKey(cwd) {
				b.WriteString("(cwd=" + cwd + ") ")
			} else {
				b.WriteString("(cwd withheld: unprintable) ")
			}
		}
		b.WriteString(cmd)
	}
	b.WriteString("`")

	if !grantable {
		b.WriteString(" (asked every time: this command has no stable form to remember)")
	}
	if hasNonASCII(resource) || hasNonASCII(cmd) || hasNonASCII(params["cwd"]) {
		b.WriteString(" — note: contains non-ASCII characters")
	}
	if fields := strings.Fields(cmd); len(fields) > 0 {
		switch fields[0] {
		case "curl", "wget":
			// Steering, not gating. The old denylist refused these
			// outright to push the model at fetch_url; the push is
			// worth keeping, the refusal is not.
			b.WriteString(" — prefer the fetch_url tool for fetching pages")
		}
	}
	return b.String()
}

// cwdUsableInKey rejects a working directory that could make a key
// display as something other than what it is, for the same reasons
// NormaliseCommand rejects those runes in a command.
func cwdUsableInKey(cwd string) bool {
	if !strings.HasPrefix(cwd, "/") {
		// A relative cwd resolves against whatever the process
		// happens to be in, so it does not identify a directory.
		return false
	}
	for _, r := range cwd {
		if r < 0x20 || isInvisible(r) || r == ')' {
			return false
		}
	}
	return true
}
