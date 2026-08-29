package compute

import (
	"path/filepath"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// One place that answers "may this tool touch this path".
//
// There used to be four. read_file/write_file/edit_file ran the mount
// resolver, the internal-path list and the hardline floor; list_files
// and glob ran two of the three; search_files ran two while returning
// file CONTENT; and the modality trio ran none of them, bounding paths
// with a private string-prefix check instead.
//
// None of that was a decision. The order was retyped at each call site,
// and a step gets dropped when a rule lives in a convention rather than
// in a function. So it lives in a function.
//
// The chain, in order, and the order is the design:
//
//  1. mount resolver   — is this inside a declared mount, in this mode
//  2. absolute         — a relative path resolves against a cwd nobody chose
//  3. internal path    — Raft snapshots, TLS keys, the memory key
//  4. hardline floor   — the compiled-in refusals
//  5. policy.d         — the operator's per-tool confinement
//
// policy.d is LAST, and that is the whole safety argument. Steps 1-4
// are floors: no file on disk can lift them. Step 5 can only narrow
// what they already permitted.
//
// The direction matters more than it looks. policy.d is a directory of
// hot-reloaded files, and one of its search paths is under the
// operator's home — where the agent runs as that user. If a policy file
// could GRANT reach, an agent that talks somebody into running a shell
// would have a supported, documented, auto-reloading route to the
// memory key. Subtract-only means the worst a hostile policy file
// achieves is a broken tool, which is noisy and recoverable.

// pathGuard bundles what the chain needs that a package-level function
// cannot reach: the tool's name, for the policy lookup.
type pathGuard struct {
	// registry supplies PolicyFor. Nil skips step 5 — which is the
	// correct reading of a node with no registry wired (a test driving
	// a builtin directly), not an excuse to skip 1-4.
	registry *Registry
}

// activePathGuard is set once at wiring time, alongside the mount
// resolver it complements. Package-level for the same reason
// activeMountResolver is: the builtins are plain funcs registered into
// a map, and threading a struct through every one of them would be a
// larger change than the thing it carries.
var activePathGuard pathGuard

// SetPathGuardRegistry wires the tool registry the guard consults for
// per-tool policies. Called once during compute wiring.
func SetPathGuardRegistry(r *Registry) { activePathGuard.registry = r }

// guardPath runs the full chain for one tool and one path.
//
// Returns the resolved path when every step passes. On refusal it
// returns the marshalled tool error and a non-zero exit, which the
// caller returns verbatim — the messages are written for the model,
// and rewording them per call site is how two tools come to disagree
// about what the same refusal means.
func guardPath(tool, path string, need MountMode) (resolved string, payload []byte, exit int) {
	return guardPathWithin(tool, path, need, "")
}

// guardPathWithin is guardPath with an implicit root that satisfies
// step 1 on its own.
//
// It exists for the modality tools. Inbound attachments are written by
// the CHANNEL layer into AllowedRoot — the agent never chose that
// directory and cannot write outside it — and an operator who pointed
// IncomingDir somewhere that is not a declared mount had a working
// deployment before this chain existed. Requiring a mount there would
// be a new demand on existing configuration, and the failure would be
// every image, audio note and PDF silently becoming unreadable.
//
// So AllowedRoot stands in for the mount check, and ONLY for it.
// Steps 2-5 run unchanged, which is the whole point: these tools
// previously had no internal-path check, no hardline and no policy,
// and a root pointed at the wrong place could read a TLS key that
// read_file refuses.
func guardPathWithin(tool, path string, need MountMode, implicitRoot string) (resolved string, payload []byte, exit int) {
	raw := strings.TrimSpace(path)
	if raw == "" {
		p, e, _ := marshalToolError("missing_arg", "path is required",
			"pass an absolute path, or a mount-scoped one like 'workspace/notes.md'")
		return "", p, e
	}

	// 1. Mount resolver. Also expands a mount label ("workspace/x")
	//    into a real path, so everything below sees the same string
	//    the filesystem will.
	//
	//    Skipped when the path is already inside implicitRoot, which
	//    is itself an operator-set bound — see guardPathWithin.
	if within, abs := insideImplicitRoot(raw, implicitRoot); within {
		resolved = abs
	} else {
		var p []byte
		var e int
		resolved, p, e = resolveFsPathMode(raw, need)
		if e != 0 {
			return "", p, e
		}
		if resolved == "" {
			resolved = raw
		}
	}

	// 2. Absolute. After resolution, because the mount form is
	//    legitimately relative and only becomes absolute here.
	if !filepath.IsAbs(resolved) {
		p, e, _ := marshalToolError("relative_path",
			"path must be absolute OR mount-scoped (e.g. 'workspace/notes.md')",
			"prefix with / for absolute, or use a mount label (see debug_storage for known mounts)")
		return "", p, e
	}

	// 3. Cluster-internal state. A floor: there is no mount
	//    configuration and no policy file that makes this reachable.
	if isInternalPath(resolved) {
		p, e, _ := marshalToolError("internal_path",
			resolved+" is cluster-internal and cannot be "+accessVerb(need),
			"this path holds private state (Raft snapshot, TLS key, memory key). There is no configuration that permits it; do not look for another path to the same file")
		return "", p, e
	}

	// 4. The compiled-in hardline floor.
	if p, e, refused := hardlinePathRefusal(resolved, accessVerb(need)); refused {
		return "", p, e
	}

	// 5. The operator's policy for THIS tool. Narrowing only.
	if pol := activePathGuard.policyFor(tool); pol != nil {
		if !pol.AllowsPath(resolved, sandboxAccess(need)) {
			p, e, _ := marshalToolError("policy_denied",
				resolved+" is outside what policy.d permits "+tool+" to "+accessVerb(need),
				"the operator confined this tool in policy.d/"+tool+".toml. Use a path that policy allows, or tell the user which path you need and why")
			return "", p, e
		}
	}
	return resolved, nil, 0
}

func (g pathGuard) policyFor(tool string) *sandbox.Policy {
	if g.registry == nil {
		return nil
	}
	return g.registry.PolicyFor(tool)
}

// sandboxAccess converts the mount vocabulary to the sandbox one. The
// two are the same three bits; the conversion exists because compute
// imports sandbox and not the reverse.
func sandboxAccess(m MountMode) sandbox.Access {
	var a sandbox.Access
	if m.Read {
		a |= sandbox.AccessR
	}
	if m.Write {
		a |= sandbox.AccessW
	}
	if m.Exec {
		a |= sandbox.AccessX
	}
	return a
}

// accessVerb renders the mode as something a refusal message can end
// with. "cannot be read" beats "cannot be accessed with mode {r:true}".
func accessVerb(m MountMode) string {
	switch {
	case m.Write && m.Read:
		return "read or written"
	case m.Write:
		return "written"
	case m.Exec:
		return "executed"
	default:
		return "read"
	}
}

// guardRead and guardWrite are the two shapes every caller actually
// wants, named so a call site says which direction it is going. A
// builtin passing the wrong MountMode is the bug this prevents: it
// reads as correct, and the mount's write bit goes unchecked.
func guardRead(tool, path string) (string, []byte, int) {
	return guardPath(tool, path, MountMode{Read: true})
}

func guardWrite(tool, path string) (string, []byte, int) {
	return guardPath(tool, path, MountMode{Read: true, Write: true})
}

// pathWithinRoot is the separator-aware containment test the modality
// builtins use for their extra AllowedRoot narrowing.
//
// Separator-aware because a bare strings.HasPrefix lets "/workspaces"
// pass a check for "/workspace" — the classic way a boundary leaks to
// the directory next door. It was written that way in three files;
// this is one of them, correct once.
func pathWithinRoot(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// insideImplicitRoot reports whether raw resolves inside root, and
// returns the absolute form when it does.
//
// filepath.Abs is what the modality tools used before this chain, so a
// relative path keeps resolving the way it always did — and it is
// bounded, because the result still has to land inside root. Empty
// root is no root: it never matches, and the caller falls through to
// the mount resolver.
func insideImplicitRoot(raw, root string) (bool, string) {
	if strings.TrimSpace(root) == "" {
		return false, ""
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return false, ""
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, ""
	}
	if !pathWithinRoot(abs, rootAbs) {
		return false, ""
	}
	return true, abs
}

// guardReadWithin is guardRead for a tool that carries its own root.
func guardReadWithin(tool, path, implicitRoot string) (string, []byte, int) {
	return guardPathWithin(tool, path, MountMode{Read: true}, implicitRoot)
}
