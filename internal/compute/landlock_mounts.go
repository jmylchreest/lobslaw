package compute

import (
	"fmt"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
)

// LandlockMounts builds a sandbox.PolicyMount slice from the active
// MountResolver. One PolicyMount per registered storage mount,
// carrying the mount root + the mode bits the operator declared.
//
// shell_command consumes the unfiltered list; skill invocations
// filter via FilterMountsForSkill so a skill can't claim more than
// it asked for in its manifest. Callers running on a node without
// any mounts get an empty slice and must decide whether to refuse
// the operation or run unsandboxed.
func LandlockMounts() []sandbox.PolicyMount {
	if activeMountResolver == nil {
		return nil
	}
	mounts := activeMountResolver.Mounts()
	out := make([]sandbox.PolicyMount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, sandbox.PolicyMount{
			Path:  m.Root,
			Read:  m.Mode.Read,
			Write: m.Mode.Write,
			Exec:  m.Mode.Exec,
		})
	}
	return out
}

// LandlockMountsByLabel returns the mode bits for the named storage
// mount, suitable for building a sandbox.PolicyMount. Returns false
// when the label isn't registered — callers should treat that as
// a configuration error (the manifest references a mount the
// operator hasn't wired).
func LandlockMountsByLabel(label string) (sandbox.PolicyMount, bool) {
	if activeMountResolver == nil {
		return sandbox.PolicyMount{}, false
	}
	for _, m := range activeMountResolver.Mounts() {
		if m.Label == label {
			return sandbox.PolicyMount{
				Path:  m.Root,
				Read:  m.Mode.Read,
				Write: m.Mode.Write,
				Exec:  m.Mode.Exec,
			}, true
		}
	}
	return sandbox.PolicyMount{}, false
}

// ModeForLabel implements the skills.MountResolver interface so the
// skill invoker can validate manifests against the live mount table
// without taking a compute → skills dependency.
func (r *MountResolver) ModeForLabel(label string) (root string, read, write, exec, ok bool) {
	if r == nil {
		return "", false, false, false, false
	}
	for _, m := range r.Mounts() {
		if m.Label == label {
			return m.Root, m.Mode.Read, m.Mode.Write, m.Mode.Exec, true
		}
	}
	return "", false, false, false, false
}

// IntersectMode caps requested bits by what mountMode grants. When
// requested asks for write or exec the mount doesn't grant, returns
// an error so a skill manifest with insufficient privileges fails
// loud at boot rather than silently dropping the request.
func IntersectMode(label string, requested, granted sandbox.PolicyMount) (sandbox.PolicyMount, error) {
	out := sandbox.PolicyMount{Path: granted.Path}
	if requested.Read && !granted.Read {
		return out, fmt.Errorf("mount %q: requested read but mount has no read", label)
	}
	if requested.Write && !granted.Write {
		return out, fmt.Errorf("mount %q: requested write but mount mode is %s", label, modeString(granted))
	}
	if requested.Exec && !granted.Exec {
		return out, fmt.Errorf("mount %q: requested exec but mount mode is %s", label, modeString(granted))
	}
	out.Read = requested.Read && granted.Read
	out.Write = requested.Write && granted.Write
	out.Exec = requested.Exec && granted.Exec
	return out, nil
}

func modeString(m sandbox.PolicyMount) string {
	s := ""
	if m.Read {
		s += "r"
	}
	if m.Write {
		s += "w"
	}
	if m.Exec {
		s += "x"
	}
	if s == "" {
		return "-"
	}
	return s
}
