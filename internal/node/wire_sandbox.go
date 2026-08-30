package node

import (
	"context"
	"log/slog"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/internal/tools"
)

// toolPolicySink routes a loaded policy to whichever sink actually
// enforces it for that tool, and refuses one it cannot enforce.
//
// Tools reach the sandbox two different ways:
//
//   - Registry.SetPolicy, for tools the Executor spawns via exec. It
//     resolves tool-specific → fleet-default → nil at invoke time.
//   - tools.SetShellPolicyOverlay, for shell_command. Builtins
//     dispatch in-process and never pass through the Executor's exec
//     path, so a registry policy would be set and silently unused —
//     which is what applyOperatorPolicies did to it before this
//     existed, meaning the one tool an operator most wants to confine
//     was the one their file could not reach.
//
// It implements sandbox.PolicySink so the boot-time load and the
// hot-reload watcher apply policies through the same code. Two paths
// with the same job is how they come to disagree about which tool gets
// what.
type toolPolicySink struct {
	registry *tools.Registry
	log      *slog.Logger
}

// SetPolicy applies p to name, or clears it when p is nil (the
// watcher's "policy file was deleted" signal).
//
// A policy that cannot be enforced is logged and dropped, leaving
// whatever was already in force. That matches applyOperatorPolicies'
// existing stance — a malformed policy must be loud but must not stop
// a node booting — and it is the safe direction on reload too: the
// alternative is widening to nothing because the new file was bad.
func (s toolPolicySink) SetPolicy(name string, p *sandbox.Policy) {
	if p != nil {
		if err := s.reject(name, p); err != nil {
			s.log.Error("sandbox: policy cannot be enforced; that tool keeps what it had",
				"tool", name, "err", err)
			return
		}
	}
	if name == tools.ShellToolName {
		tools.SetShellPolicyOverlay(p)
		return
	}
	s.registry.SetPolicy(name, p)
}

// reject reports why p cannot be applied to name, or nil when it can.
func (s toolPolicySink) reject(name string, p *sandbox.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if name != tools.ShellToolName {
		return nil
	}
	if unsupported := tools.UnsupportedShellPolicyFields(p); len(unsupported) > 0 {
		return &unenforceableError{fields: unsupported}
	}
	return nil
}

// unenforceableError names the fields that stopped a policy applying,
// so the log line an operator reads says which key to remove rather
// than that something, somewhere, was wrong.
type unenforceableError struct{ fields []string }

func (e *unenforceableError) Error() string {
	return "shell_command takes paths and seccomp only; cannot enforce " +
		joinFields(e.fields) + " — remove them rather than run with a control that isn't there"
}

func joinFields(fields []string) string {
	out := ""
	for i, f := range fields {
		if i > 0 {
			out += ", "
		}
		out += f
	}
	return out
}

func (n *Node) policySink() toolPolicySink {
	return toolPolicySink{registry: n.toolRegistry, log: n.log}
}

// startSandboxWatcher subscribes to the policy directories so an
// edited policy.d file takes effect without a restart.
//
// The watcher existed, was tested, and was called by nothing — so the
// hot-reload behaviour SANDBOX.md documents was a claim the code did
// not make. An operator who tightened a policy and waited for it to
// take hold waited forever.
//
// Failing to subscribe is not fatal: applyOperatorPolicies already
// loaded every policy synchronously, so the node is enforcing what the
// files said. What is lost is freshness, and that is worth a warning
// rather than a refusal to start.
func (n *Node) startSandboxWatcher(ctx context.Context) {
	if n.toolRegistry == nil || len(n.cfg.SandboxPolicyDirs) == 0 {
		return
	}
	watcher := sandbox.NewWatcherMulti(
		n.cfg.SandboxPolicyDirs,
		n.policySink(),
		sandbox.LoadOptions{Logger: n.log},
		0,
	)
	if err := watcher.Start(ctx); err != nil {
		n.log.Warn("sandbox watcher: not subscribed; policies are boot-time only",
			"dirs", n.cfg.SandboxPolicyDirs, "err", err)
		return
	}
	n.log.Info("sandbox watcher: subscribed", "dirs", n.cfg.SandboxPolicyDirs)
}
