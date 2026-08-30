package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
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
	if err := b.Register("remote_ssh", newRemoteSSHHandler(set)); err != nil {
		return err
	}
	return b.Register("remote_scp", newRemoteSCPHandler(set))
}

func newRemoteSSHHandler(set *RemoteSet) compute.BuiltinFunc {
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
			Path:        compute.BuiltinScheme + "remote_ssh",
			Description: desc,
			ParametersSchema: fmt.Appendf(nil, `{
				"type": "object",
				"properties": {
					"remote": {"type": "string", "enum": [%s], "description": "Which remote to run on."},
					"command": {"type": "string", "description": "The command to run. A login shell runs it, so pipes and redirection work."},
					"cwd": {"type": "string", "description": "Optional working directory on the remote, e.g. /workspace/tasks/my-task."},
					"timeout_secs": {"type": "integer", "description": "Optional. Seconds to allow before giving up. Builds are slow; the default is minutes."}
				},
				"required": ["remote", "command"],
				"additionalProperties": false
			}`, jsonStringList(names)),
			// Irreversible: the command can push a branch, delete a
			// working tree, or install packages. That it happens on a
			// disposable host bounds the blast radius; it does not make
			// the action reversible, and the risk tier is about the
			// action rather than about where it lands.
			//
			// Work that belongs on another machine does not belong in
			// the local shell, and the split is structural:
			// shell_command runs inside THIS process, next to the model
			// keys and the cluster's mTLS material.
			AvoidTools: []string{"shell_command"},
			RiskTier:   types.RiskIrreversible,
		},
		remoteSCPToolDef(set),
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

// remote_scp: moving a file between here and there.
//
// A SEPARATE TOOL FROM remote_ssh, and separately riskier. remote_ssh
// runs a command over there; this one touches the local filesystem,
// and the local filesystem is where the cluster CA, the node key and
// the memory key live.
//
// So it reuses the fs builtins' guards rather than inventing its own —
// in the direction that matches what it is about to do:
//
//	upload   reads local, writes remote  -> the EXFILTRATION direction
//	download writes local, reads remote  -> the OVERWRITE direction
//
// Getting the direction wrong would apply a read check to a write, and
// the mount resolver's write bit would go unchecked. That is why the
// direction is resolved first and the guards are chosen from it,
// rather than a single "check the path" call at the top.

func newRemoteSCPHandler(set *RemoteSet) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		box, err := set.Get(args["remote"])
		if err != nil {
			return nil, 2, err
		}
		remotePath := strings.TrimSpace(args["remote_path"])
		localRaw := strings.TrimSpace(args["local_path"])
		if remotePath == "" || localRaw == "" {
			return nil, 2, errors.New("remote_scp: remote_path and local_path are both required")
		}

		switch strings.ToLower(strings.TrimSpace(args["direction"])) {
		case "upload":
			return remoteUpload(ctx, box, localRaw, remotePath)
		case "download":
			return remoteDownload(ctx, box, localRaw, remotePath)
		case "":
			return nil, 2, errors.New(`remote_scp: direction is required ("upload" or "download")`)
		default:
			return nil, 2, fmt.Errorf("remote_scp: unknown direction %q (want \"upload\" or \"download\")", args["direction"])
		}
	}
}

// remoteUpload sends a local file out. Every guard here is a READ
// guard, because that is what this does locally — and reading a file
// in order to put it on another machine is the sharpest form of
// reading one.
func remoteUpload(ctx context.Context, box *Remote, localRaw, remotePath string) ([]byte, int, error) {
	local, payload, exit := resolveFsPath(localRaw, false)
	if exit != 0 {
		return payload, exit, nil
	}
	if local == "" {
		local = localRaw
	}
	if !filepath.IsAbs(local) {
		return compute.MarshalToolError("relative_path", "local_path must be absolute OR mount-scoped (e.g. 'workspace/out.log')",
			"prefix with / for absolute, or use a mount label (see debug_storage for known mounts)")
	}
	if payload, exit, refused := compute.HardlinePathRefusal(local, "sent to a remote"); refused {
		return payload, exit, nil
	}

	body, err := os.ReadFile(local)
	if err != nil {
		return nil, 1, fmt.Errorf("remote_scp: read %s: %w", local, err)
	}
	if err := box.Put(ctx, remotePath, body); err != nil {
		return nil, 1, err
	}
	return marshalTransfer("upload", box.Name, local, remotePath, len(body))
}

// remoteDownload pulls a remote file in. Every guard here is a WRITE
// guard: the danger is not what the remote holds, it is what this
// overwrites — a config, a cert, a skill the agent then executes.
func remoteDownload(ctx context.Context, box *Remote, localRaw, remotePath string) ([]byte, int, error) {
	local, payload, exit := resolveFsPath(localRaw, true)
	if exit != 0 {
		return payload, exit, nil
	}
	if local == "" {
		local = localRaw
	}
	if !filepath.IsAbs(local) {
		return compute.MarshalToolError("relative_path", "local_path must be absolute OR mount-scoped (e.g. 'workspace/out.log')",
			"prefix with / for absolute, or use a mount label (see debug_storage for known mounts)")
	}
	if payload, exit, refused := compute.HardlinePathRefusal(local, "overwritten from a remote"); refused {
		return payload, exit, nil
	}

	body, err := box.Get(ctx, remotePath)
	if err != nil {
		return nil, 1, err
	}
	// 0o600 rather than 0o644: the bytes came from another machine and
	// nothing here has looked at them.
	if err := os.WriteFile(local, body, 0o600); err != nil {
		return nil, 1, fmt.Errorf("remote_scp: write %s: %w", local, err)
	}
	return marshalTransfer("download", box.Name, local, remotePath, len(body))
}

func marshalTransfer(direction, remote, local, remotePath string, n int) ([]byte, int, error) {
	out, err := json.Marshal(map[string]any{
		"direction":   direction,
		"remote":      remote,
		"local_path":  local,
		"remote_path": remotePath,
		"bytes":       n,
	})
	if err != nil {
		return nil, 1, err
	}
	return out, 0, nil
}

// remoteSCPToolDef is registered alongside remote_ssh. Both are behind
// the same `remote_*` glob, so a deployment that enables one gets both
// unless it names the other in disabled_tools — which is exactly what
// `disabled_tools = ["remote_scp"]` is for.
func remoteSCPToolDef(set *RemoteSet) *types.ToolDef {
	return &types.ToolDef{
		Name: "remote_scp",
		Path: compute.BuiltinScheme + "remote_scp",
		Description: "Copy ONE file between this machine and a configured remote. " +
			"Use it for a log, a patch or a small artefact — a repository moves by git, not by this. " +
			"local_path is subject to the same mount and refusal rules as read_file and write_file, " +
			"so cluster-internal paths are refused in both directions. Configured remotes:" + set.Describe(),
		ParametersSchema: fmt.Appendf(nil, `{
			"type": "object",
			"properties": {
				"remote": {"type": "string", "enum": [%s], "description": "Which remote."},
				"direction": {"type": "string", "enum": ["upload", "download"], "description": "upload sends local_path to the remote; download fetches remote_path to local_path."},
				"local_path": {"type": "string", "description": "Path on this machine. Absolute, or mount-scoped like 'workspace/out.log'."},
				"remote_path": {"type": "string", "description": "Path on the remote."}
			},
			"required": ["remote", "direction", "local_path", "remote_path"],
			"additionalProperties": false
		}`, jsonStringList(set.Names())),
		RiskTier: types.RiskIrreversible,
	}
}
