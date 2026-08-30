package tools

import (
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/policy"
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
// Three questions, in order, and the order is the design:
//
//  1. mount resolver  — does this path exist to the agent, in this
//                       mode? (absoluteness falls out of this step)
//  2. hardline floor  — policy.CheckPath. Allow, confirm, or deny.
//  3. policy.d        — may THIS tool touch it?
//
// Steps 1 and 2 grant and refuse respectively; step 3 can only narrow
// what step 1 already permitted, and that is the whole safety
// argument.
//
// The direction matters more than it looks. policy.d is a directory of
// hot-reloaded files, and one of its search paths is under the
// operator's home — where the agent runs as that user. If a policy file
// could GRANT reach, an agent that talks somebody into running a shell
// would have a supported, documented, auto-reloading route to the
// memory key. Subtract-only means the worst a hostile policy file
// achieves is a broken tool, which is noisy and recoverable.

// activePathGuard is the registry the guard consults for per-tool
// policies, set at wiring time. Package-level for the same reason
// activeMountResolver is: the builtins are plain funcs registered into
// a map, and threading a struct through every one of them would be a
// larger change than the thing it carries.
//
// ATOMIC, not a bare pointer. "Set once at wiring, read on the hot
// path" sounds single-threaded and is not: every node.New() writes it,
// and a process that builds two nodes — which every parallel test in
// internal/node does — has two goroutines writing while others read.
// The race detector called it, and it was a real one: an unsynchronised
// pointer write is not guaranteed to be visible to another goroutine at
// all, so a builtin could consult a nil registry on a node that has
// one and skip the operator's policy silently.
//
// atomic.Pointer rather than a mutex because the read is on every path
// check and the write happens once per node.
var activePathGuard atomic.Pointer[Registry]

// SetPathGuardRegistry wires the tool registry the guard consults for
// per-tool policies. Called once during compute wiring.
func SetPathGuardRegistry(r *Registry) { activePathGuard.Store(r) }

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
		p, e, _ := compute.MarshalToolError("missing_arg", "path is required",
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

	//    Absoluteness is a postcondition of this step, not a gate of
	//    its own: the mount-label form is legitimately relative and
	//    only becomes absolute once the resolver has expanded it.
	if !filepath.IsAbs(resolved) {
		p, e, _ := compute.MarshalToolError("relative_path",
			"path must be absolute OR mount-scoped (e.g. 'workspace/notes.md')",
			"prefix with / for absolute, or use a mount label (see debug_storage for known mounts)")
		return "", p, e
	}

	// 2. The compiled-in floor. One list — see policy.protectedPaths.
	//    There used to be a separate isInternalPath step in front of
	//    this, over a second list that overlapped it on state.db,
	//    *.key and *.pem while disagreeing about why. It also ran
	//    first and was a flat deny, so on every shared pattern this
	//    file's three-verdict model could never take effect.
	if p, e, refused := compute.HardlinePathRefusal(resolved, accessVerb(need)); refused {
		return "", p, e
	}

	// 3. The operator's policy for THIS tool. Narrowing only.
	if pol := policyFor(tool); pol != nil {
		if !pol.AllowsPath(resolved, sandboxAccess(need)) {
			p, e, _ := compute.MarshalToolError("policy_denied",
				resolved+" is outside what policy.d permits "+tool+" to "+accessVerb(need),
				"the operator confined this tool in policy.d/"+tool+".toml. Use a path that policy allows, or tell the user which path you need and why")
			return "", p, e
		}
	}
	return resolved, nil, 0
}

// policyFor returns the tool's policy, or nil when no registry is
// wired — which is the correct reading of a test driving a builtin
// directly, and skips step 3 only.
func policyFor(tool string) *sandbox.Policy {
	r := activePathGuard.Load()
	if r == nil {
		return nil
	}
	return r.PolicyFor(tool)
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

// isInternalPath reports whether a path is hidden from LISTINGS.
//
// One list now: policy.CheckPath, the compiled-in floor. There used to
// be a second — internalExcludes, right here — which overlapped the
// floor on state.db, *.key and *.pem, missed everything the floor
// caught (~/.ssh, ~/.aws, /etc/shadow, .env), and blocked .git
// wholesale because it was written for lobslaw's own data directory.
//
// Listing is where a floor becomes a FILTER rather than a refusal:
// list_files does not fail because a directory contains a key, it
// just does not show it. That is the only reason this wrapper exists
// rather than the callers asking CheckPath themselves.
//
// PathDenied only, deliberately. A confirm-tier path (~/.ssh/config)
// is sensitive-but-answerable, and hiding it from a listing would
// mean the user can never discover the file they would be approving.
func isInternalPath(path string) bool {
	verdict, _ := policy.CheckPath(path)
	return verdict == policy.PathDenied
}
