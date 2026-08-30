package compute

import (
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/policy"
)

// commandParams are the parameter names that carry something a shell
// will execute. Checked by name rather than by tool, so a tool
// registered later gets the floor for free instead of only when
// somebody remembers to add it here.
var commandParams = []string{"command", "cmd", "script"}

// pathParams are the parameter names that carry a filesystem path.
var pathParams = []string{"path", "file", "file_path", "dir", "directory", "src", "dst", "destination"}

// hardlineCheck runs the compiled-in floor over a tool invocation's
// parameters.
//
// Param-name driven because the executor sees a generic
// map[string]string and cannot know a tool's semantics. That is coarse
// — a tool naming its command parameter something else slips past —
// which is why the shell and fs builtins check again themselves. The
// floor is a stop signal and an accident guard, not the sandbox.
func hardlineCheck(params map[string]string) error {
	for _, key := range commandParams {
		cmd, ok := params[key]
		if !ok {
			continue
		}
		if err := policy.CheckCommand(cmd); err != nil {
			return err
		}
		if err := policy.CheckCommandPaths(cmd); err != nil {
			return err
		}
	}
	for _, key := range pathParams {
		p, ok := params[key]
		if !ok {
			continue
		}
		// Denials only. A confirm verdict is handled after policy has
		// had its say — see hardlineConfirm.
		if verdict, err := policy.CheckPath(p); verdict == policy.PathDenied {
			return err
		}
	}
	return nil
}

// HardlinePathRefusal returns the structured tool error for a path the
// floor denies, or ok=false when it has no objection.
//
// The fs builtins call this themselves as well as being covered by the
// executor, for the same reason the shell builtin does: the executor
// check cannot be bypassed by a new caller, and this one still fires
// if some future path reaches the builtin directly.
func HardlinePathRefusal(path, verb string) (payload []byte, exit int, ok bool) {
	verdict, err := policy.CheckPath(path)
	if verdict != policy.PathDenied {
		return nil, 0, false
	}
	p, e, _ := MarshalToolError("hardline_refused",
		path+" cannot be "+verb+": "+err.Error(),
		"this refusal is compiled in and there is no configuration that permits it; do not retry, and do not look for another way to reach the same file")
	return p, e, true
}

// hardlineConfirm escalates a sensitive-but-not-secret path to a
// confirmation.
//
// Runs AFTER policy rather than before, so a path the operator's rules
// already deny is refused outright instead of prompting the user about
// something that was never going to be permitted. A floor can only
// raise the bar, never lower it, so escalating a policy allow to a
// confirmation is the one direction this is allowed to move.
func hardlineConfirm(params map[string]string) error {
	for _, key := range pathParams {
		p, ok := params[key]
		if !ok {
			continue
		}
		verdict, hErr := policy.CheckPath(p)
		if verdict != policy.PathConfirm {
			continue
		}
		return fmt.Errorf("%w: %s", ErrRequireConfirm, hErr.Error())
	}
	return nil
}
