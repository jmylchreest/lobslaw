package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// remote_ssh: the agent's hands, somewhere that is not this process.
//
// Registered only when [[remote]] blocks exist. A node with none does
// not advertise a tool that cannot work — the model reading a remote
// tool in its list and finding every call refused is worse than not
// seeing one, because it will keep trying.

// RegisterRemoteBuiltins installs remote_ssh.
//
// NOTE for the policy seed: remote_ssh is in wire_seeds.go's
// noSeedTools, so it gets neither a default-allow nor a default-deny.
// Builtins are seeded default-allow on the grounds that they are
// lobslaw-curated with a well-understood blast radius. That is true of
// read_file. It is not true of a tool whose entire purpose is to run
// commands the model composed on a machine with a git push token.
func RegisterRemoteBuiltins(b *Builtins, set *RemoteSet) error {
	if set == nil || len(set.Names()) == 0 {
		return errors.New("remote tools: at least one configured remote is required")
	}
	return b.Register("remote_ssh", newRemoteSSHHandler(set))
}

func newRemoteSSHHandler(set *RemoteSet) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		box, err := set.Get(args["remote"])
		if err != nil {
			// Exit 2, not 1: this is a malformed call, and the message
			// names what IS configured so the model's retry can be
			// right rather than another guess.
			return nil, 2, err
		}
		command := strings.TrimSpace(args["command"])
		if command == "" {
			return nil, 2, errors.New("remote_ssh: command is required")
		}

		res, err := box.Exec(ctx, command, args["cwd"], remoteTimeoutArg(args["timeout_secs"]))
		if err != nil {
			// Transport failure. Distinct from a command that ran and
			// failed, which comes back below with its exit code — the
			// model's next move differs completely between the two.
			return nil, 1, err
		}

		out, merr := json.Marshal(res)
		if merr != nil {
			return nil, 1, merr
		}
		// Exit 0 even when the command failed. The tool did its job;
		// the result reports what happened. A non-zero here would have
		// the agent retrying the connection instead of reading the
		// compiler output it just asked for.
		return out, 0, nil
	}
}

// remoteTimeoutArg parses the model's timeout. Anything unparseable is
// zero, which Exec reads as "use the configured default" — a mistyped
// timeout should not be a refusal.
func remoteTimeoutArg(raw string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// RemoteToolDefs are the ToolDefs to register alongside the builtin.
//
// The description carries the configured remotes by name, because the
// alternative is the model guessing one and being corrected — which
// costs a turn and reads to the user as the agent not knowing its own
// deployment.
func RemoteToolDefs(set *RemoteSet) []*types.ToolDef {
	names := set.Names()
	if len(names) == 0 {
		return nil
	}
	desc := "Run a command on an isolated development host over SSH, and return its output. " +
		"This is where build, test, dependency-install and code-editing work belongs — NOT " +
		"shell_command, which runs inside the agent's own process alongside its credentials. " +
		"A non-zero exit_code is a result, not a failure: read stderr and act on it rather than " +
		"retrying the call unchanged. Configured remotes:" + set.Describe()

	return []*types.ToolDef{
		{
			Name:        "remote_ssh",
			Path:        BuiltinScheme + "remote_ssh",
			Description: desc,
			ParametersSchema: []byte(fmt.Sprintf(`{
				"type": "object",
				"properties": {
					"remote": {"type": "string", "enum": [%s], "description": "Which remote to run on."},
					"command": {"type": "string", "description": "The command to run. A login shell runs it, so pipes and redirection work."},
					"cwd": {"type": "string", "description": "Optional working directory on the remote, e.g. /workspace/tasks/my-task."},
					"timeout_secs": {"type": "integer", "description": "Optional. Seconds to allow before giving up. Builds are slow; the default is minutes."}
				},
				"required": ["remote", "command"],
				"additionalProperties": false
			}`, jsonStringList(names))),
			// Irreversible: the command can push a branch, delete a
			// working tree, or install packages. That it happens on a
			// disposable host bounds the blast radius; it does not make
			// the action reversible, and the risk tier is about the
			// action rather than about where it lands.
			RiskTier: types.RiskIrreversible,
		},
	}
}

// jsonStringList renders names as a JSON array body, so the schema can
// constrain `remote` to what actually exists. An enum rather than a
// free string is the same argument the whole feature rests on: the set
// of reachable machines is the operator's, not the model's.
func jsonStringList(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		b, err := json.Marshal(n)
		if err != nil {
			continue
		}
		quoted = append(quoted, string(b))
	}
	return strings.Join(quoted, ", ")
}
