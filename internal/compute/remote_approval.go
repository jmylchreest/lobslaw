package compute

import (
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// RemoteApprovalDefault gates remote_ssh per command, the way
// ShellApprovalDefault gates the local shell.
//
// A default rather than a compiled-in refusal, so an operator who
// wants a standing grant writes a rule instead of a patch:
//
//	action = "remote:run"
//	resource = "(remote=*) git *"
func RemoteApprovalDefault() types.PolicyRule {
	return types.PolicyRule{
		ID:       "default-remote-run-confirm",
		Subject:  "*",
		Action:   RemoteAction,
		Resource: "*",
		Effect:   types.EffectRequireConfirmation,
		Priority: 1,
	}
}

// RemoteCopyApprovalDefault does the same for remote_scp. Its own rule
// because a file copy is not a command: approving `git *` on a host
// must not license pulling /etc/shadow off it.
func RemoteCopyApprovalDefault() types.PolicyRule {
	return types.PolicyRule{
		ID:       "default-remote-copy-confirm",
		Subject:  "*",
		Action:   RemoteCopyAction,
		Resource: "*",
		Effect:   types.EffectRequireConfirmation,
		Priority: 1,
	}
}

// RemoteHostLookup resolves a configured remote's name to its host.
type RemoteHostLookup func(name string) (host string, ok bool)

// RemoteGrantResourceFor derives what a remote_ssh call is granted
// under, keyed on the HOST rather than the configured name.
//
// The name is what the model passes, because a name is what it can
// reason about. The key has to be the host, because that is the only
// form the other route to the same operation knows: shell_command
// running `ssh 10.0.0.5 uptime` has a host and no idea the operator
// calls it "e2e". Keying on the name would leave two keys for one
// operation and an operator who granted one having said nothing about
// the other — which is the entire property this exists for.
func RemoteGrantResourceFor(hostOf RemoteHostLookup) func(map[string]string) GrantTarget {
	return func(params map[string]string) GrantTarget {
		return remoteGrant(hostOf, params)
	}
}

func remoteGrant(hostOf RemoteHostLookup, params map[string]string) GrantTarget {
	host, ok := resolveRemoteHost(hostOf, params["remote"])
	if !ok {
		return GrantTarget{Action: RemoteAction, Resource: UnclassifiedResource}
	}
	key, kok := NormaliseCommand(params["command"])
	if !kok {
		return GrantTarget{Action: RemoteAction, Resource: UnclassifiedResource}
	}
	return remoteTarget(RemoteAction, host, key)
}

// resolveRemoteHost maps the name the model passed to the host the key
// is written about. An unknown name yields no host: the enum in the
// tool schema should make that unreachable, and a key naming a remote
// we cannot resolve would be a grant about nothing.
func resolveRemoteHost(hostOf RemoteHostLookup, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	if hostOf == nil {
		return "", false
	}
	host, ok := hostOf(name)
	host = strings.TrimSpace(host)
	if !ok || host == "" {
		return "", false
	}
	return host, true
}

// RemoteCopyGrantResourceFor derives what a remote_scp call is granted
// under, keyed on the host for the reason RemoteGrantResourceFor is.
//
// The direction is in the key because it is the difference between
// sending a file and taking one, and an approval for one must not
// cover the other. The remote path is in it because which file is the
// whole question.
func RemoteCopyGrantResourceFor(hostOf RemoteHostLookup) func(map[string]string) GrantTarget {
	return func(params map[string]string) GrantTarget {
		return remoteCopyGrant(hostOf, params)
	}
}

func remoteCopyGrant(hostOf RemoteHostLookup, params map[string]string) GrantTarget {
	host, hok := resolveRemoteHost(hostOf, params["remote"])
	direction := strings.ToLower(strings.TrimSpace(params["direction"]))
	remotePath := strings.TrimSpace(params["remote_path"])
	if !hok || remotePath == "" || (direction != "upload" && direction != "download") {
		return GrantTarget{Action: RemoteCopyAction, Resource: UnclassifiedResource}
	}
	return remoteTarget(RemoteCopyAction, host, direction+" "+remotePath)
}

// RemoteCommandSummary renders a remote_ssh call for the prompt.
// Verbatim and in full, for the reason ShellCommandSummary is: a
// prompt that paraphrases what is about to run cannot be answered.
func RemoteCommandSummary(params map[string]string) string {
	cmd := strings.TrimSpace(params["command"])
	host := strings.TrimSpace(params["remote"])
	if cmd == "" || host == "" {
		return ""
	}
	return "run `" + cmd + "` on " + host
}

// RemoteCopySummary renders a remote_scp call for the prompt.
func RemoteCopySummary(params map[string]string) string {
	host := strings.TrimSpace(params["remote"])
	remotePath := strings.TrimSpace(params["remote_path"])
	localPath := strings.TrimSpace(params["local_path"])
	switch strings.ToLower(strings.TrimSpace(params["direction"])) {
	case "upload":
		return "send `" + localPath + "` to " + host + ":" + remotePath
	case "download":
		return "fetch " + host + ":" + remotePath + " to `" + localPath + "`"
	}
	return ""
}

// NetFetchApprovalDefault gates commands that retrieve over the
// network — curl, wget and anything the operator classifies that way.
//
// Seeded whenever the shell is registered, not only when a remote is
// declared: shell_command can run curl on a node with no [[remote]] at
// all, and without a default for the action it resolves to, the call
// would hit default-deny instead of asking. That is a refusal the
// operator never wrote and cannot see the reason for.
func NetFetchApprovalDefault() types.PolicyRule {
	return types.PolicyRule{
		ID:       "default-net-fetch-confirm",
		Subject:  "*",
		Action:   NetFetchAction,
		Resource: "*",
		Effect:   types.EffectRequireConfirmation,
		Priority: 1,
	}
}
