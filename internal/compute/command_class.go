package compute

import (
	"net/url"
	"strings"
	"sync/atomic"
)

// Commands that reach off the box are governed by what they do, not by
// which tool ran them.
//
// shell_command can run `ssh web01 git pull`, and remote_ssh can run
// the same thing. Without classification those are two resources under
// two actions, so an operator who grants one has said nothing about the
// other — and the substring denylist that used to stop shell_command
// reaching ssh at all is gone, replaced by per-command approval. One
// action per kind of reaching-out means a single rule governs both
// routes.
//
// This is classification, not denial. Nothing here refuses a command
// or forces a prompt; it decides which rule is consulted. That
// distinction is why it does not re-create the problem the denylist
// had, where the only way to permit one ssh was to edit Go.
const (
	// RemoteAction covers executing a command on another host.
	RemoteAction = "remote:run"

	// RemoteCopyAction covers moving a file to or from another host.
	// Separate from RemoteAction because they are different
	// permissions: approving `git *` on a host must not also license
	// pulling /etc/shadow off it.
	RemoteCopyAction = "remote:copy"

	// NetFetchAction covers retrieving over the network. Separate
	// again — a tool that fetches a URL is not one that runs code
	// somewhere.
	NetFetchAction = "net:fetch"
)

// HostFrom names how a command's target host is found in its argv.
//
// Three shapes rather than an operator-supplied regex. A regex that
// mis-extracts does not fail, it produces a grant naming the WRONG
// host — an approval for web01 silently applied to db02 — and that is
// not a failure mode worth exposing as a config knob.
type HostFrom string

const (
	// HostFromSSH: the first non-flag token, minus any user@ prefix.
	// Flags that take a value consume the token after them.
	HostFromSSH HostFrom = "ssh-style"

	// HostFromColonPath: the first token containing a colon, taking
	// what precedes it — scp and rsync's host:path form.
	HostFromColonPath HostFrom = "scp-style"

	// HostFromURL: the first token that parses as a URL with a host.
	HostFromURL HostFrom = "url-style"
)

// CommandClass is how one command name is governed.
type CommandClass struct {
	Action   string
	HostFrom HostFrom
}

// DefaultCommandClasses is the shipped table. Operators extend or
// override it via [compute.command_classes]; an empty action there
// means "do not classify", which is how somebody disagrees with an
// entry without patching Go.
var DefaultCommandClasses = map[string]CommandClass{
	"ssh":    {Action: RemoteAction, HostFrom: HostFromSSH},
	"scp":    {Action: RemoteCopyAction, HostFrom: HostFromColonPath},
	"rsync":  {Action: RemoteCopyAction, HostFrom: HostFromColonPath},
	"rclone": {Action: RemoteCopyAction, HostFrom: HostFromURL},
	"curl":   {Action: NetFetchAction, HostFrom: HostFromURL},
	"wget":   {Action: NetFetchAction, HostFrom: HostFromURL},
}

// sshValueFlags take the next token as their value, so the token after
// them is not the host. Long forms (--option=value) carry their value
// inline and need no entry.
var sshValueFlags = map[string]bool{
	"-p": true, "-i": true, "-o": true, "-l": true,
	"-F": true, "-J": true, "-b": true, "-c": true,
	"-D": true, "-L": true, "-R": true, "-W": true, "-E": true,
}

// ClassifyCommand decides which action governs a command, and what
// host it is aimed at.
//
// Returns ok=false when the command is not classified, which is the
// ordinary case — it means shell:run governs it as before.
//
// Returns ok=true with an empty host when the command IS classified
// but the host could not be extracted unambiguously. That is
// deliberate and the caller must treat it as not-grantable: a standing
// grant naming a host we only guessed at is worse than asking again.
func ClassifyCommand(classes map[string]CommandClass, tokens []string) (class CommandClass, host string, rest []string, ok bool) {
	if len(tokens) == 0 {
		return CommandClass{}, "", nil, false
	}
	// The command as invoked, not its path: /usr/bin/ssh is ssh.
	name := tokens[0]
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	class, found := classes[name]
	if !found || class.Action == "" {
		return CommandClass{}, "", nil, false
	}
	switch class.HostFrom {
	case HostFromSSH:
		host, rest = hostFromSSHArgv(tokens[1:])
	case HostFromColonPath:
		host = hostFromColonPath(tokens[1:])
	case HostFromURL:
		host = hostFromURL(tokens[1:])
	}
	return class, host, rest, true
}

func hostFromSSHArgv(args []string) (host string, rest []string) {
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if strings.HasPrefix(tok, "-") {
			if sshValueFlags[tok] {
				i++ // its value is not the host
			}
			continue
		}
		return stripUser(tok), args[i+1:]
	}
	return "", nil
}

func hostFromColonPath(args []string) string {
	for _, tok := range args {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		// A Windows-style drive letter is not a host, and neither is a
		// bare colon with nothing before it.
		if i := strings.IndexByte(tok, ':'); i > 1 {
			return stripUser(tok[:i])
		}
	}
	return ""
}

func hostFromURL(args []string) string {
	for _, tok := range args {
		if strings.HasPrefix(tok, "-") {
			continue
		}
		u, err := url.Parse(tok)
		if err != nil || u.Host == "" {
			continue
		}
		return u.Hostname()
	}
	return ""
}

// stripUser removes a user@ prefix. The user is not part of what is
// being reached — an approval is about the host.
func stripUser(tok string) string {
	if i := strings.LastIndexByte(tok, '@'); i >= 0 {
		return tok[i+1:]
	}
	return tok
}

// RemoteResourceKey renders the grant key for a command aimed at a
// host: "(remote=web01) git pull".
//
// The host is IN the key rather than beside it, for the reason
// ShellGrantResource already puts cwd in one — the same command
// against a different target is a different operation, and an approval
// for one must not cover the other.
func RemoteResourceKey(host, command string) string {
	return "(remote=" + host + ") " + command
}

// activeCommandClasses is the table in force. Set once at wiring time
// from config; the default table until then.
//
// A package var rather than a field threaded through ShellGrantResource,
// because the resolver signature is fixed by the gate and this is the
// same shape activeMountResolver and the path guard already use.
var activeCommandClasses atomic.Pointer[map[string]CommandClass]

// SetCommandClasses installs the operator's table. Nil or empty
// restores the shipped defaults, so clearing the config section is the
// same as never having written one.
func SetCommandClasses(m map[string]CommandClass) {
	if len(m) == 0 {
		activeCommandClasses.Store(nil)
		return
	}
	activeCommandClasses.Store(&m)
}

// ActiveCommandClasses returns the table in force.
func ActiveCommandClasses() map[string]CommandClass {
	if m := activeCommandClasses.Load(); m != nil {
		return *m
	}
	return DefaultCommandClasses
}
