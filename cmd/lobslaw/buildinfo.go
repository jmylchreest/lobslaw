package main

import (
	"runtime/debug"
	"strings"
	"time"
)

// What this binary is, and where it came from.
//
// Three package variables overwritten at link time with -X, which is
// the only mechanism that works for the builds that matter: the
// container image has no .git in its context, so nothing can be
// derived from a repository that is not there.
//
// Everything else — go build, go install, go run — gets the same
// answer from the build info the toolchain embeds automatically. That
// is the case the stamp used to miss: an unstamped binary reported
// "dev/none" while carrying the commit it was built from all along.

// Version, Commit and BuildDate are injected at build time via
// -ldflags. Defaults mark an unstamped build, which resolveBuildStamp
// then tries to fill in from the embedded build info.
var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = ""
)

// buildStamp is the resolved answer, after the fallbacks.
type buildStamp struct {
	Version string
	Commit  string
	Built   string // RFC3339 UTC, or empty when nothing knows
	// Dirty reports a build made from a tree with uncommitted
	// changes. Worth surfacing: "commit abc123" is a lie about what
	// is running when the tree had edits, and this is the field that
	// stops somebody chasing a bug through the wrong source.
	Dirty bool
}

// resolveBuildStamp fills the gaps the linker left.
//
// The stamp wins where it is set: a release names its tag, and a
// commit hash derived from vcs metadata would quietly disagree with
// it on a build made from a detached checkout.
func resolveBuildStamp() buildStamp {
	out := buildStamp{Version: Version, Commit: Commit, Built: BuildDate}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return out
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if out.Commit == "none" || out.Commit == "" {
				out.Commit = shortCommit(s.Value)
			}
		case "vcs.time":
			if out.Built == "" {
				// The COMMIT time, not a build time, and labelled as
				// such by being the fallback: a toolchain build has no
				// build timestamp to embed, because embedding one
				// would make the output differ from one minute to the
				// next for identical source.
				out.Built = s.Value
			}
		case "vcs.modified":
			out.Dirty = s.Value == "true"
		}
	}
	// A tagged install carries its version here, which is the right
	// answer for `go install ...@v1.2.3`. A pseudo-version is not: it
	// encodes the commit and its timestamp, both of which are already
	// on the line, and reads as noise where a version belongs.
	if out.Version == "dev" && isReleaseVersion(info.Main.Version) {
		out.Version = info.Main.Version
	}
	return out
}

// isReleaseVersion reports whether a module version is a tag rather
// than a pseudo-version synthesised from a commit.
func isReleaseVersion(v string) bool {
	if v == "" || v == "(devel)" {
		return false
	}
	// Pseudo-versions carry a 14-digit UTC timestamp segment:
	// v0.0.0-20260901000653-8bfa854deeab.
	for _, part := range strings.Split(v, "-") {
		if len(part) == 14 && isAllDigits(part) {
			return false
		}
	}
	return true
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// shortCommit trims a full hash to the length people quote.
func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

// String renders the stamp for `--version` and the boot log.
func (b buildStamp) String() string {
	var sb strings.Builder
	sb.WriteString(b.Version)
	sb.WriteString(" (")
	sb.WriteString(b.Commit)
	if b.Dirty {
		sb.WriteString("-dirty")
	}
	if b.Built != "" {
		sb.WriteString(", built ")
		sb.WriteString(humanBuildTime(b.Built))
	}
	sb.WriteString(")")
	return sb.String()
}

// humanBuildTime renders a timestamp the way a person reads one,
// falling back to whatever was stamped when it will not parse — a
// build date nobody can format is still a build date worth printing.
func humanBuildTime(raw string) string {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return raw
	}
	return t.UTC().Format("2006-01-02 15:04:05 MST")
}
