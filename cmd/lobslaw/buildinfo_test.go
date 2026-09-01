package main

import (
	"strings"
	"testing"
)

// The stamp wins where it is set: a release names its tag, and a
// commit derived from vcs metadata would disagree with it on a build
// made from a detached checkout.
func TestStampedValuesWin(t *testing.T) {
	t.Parallel()
	restore := stampFor(t, "v1.4.0", "abc123", "2026-09-01T00:00:00Z")
	defer restore()

	got := resolveBuildStamp()
	if got.Version != "v1.4.0" || got.Commit != "abc123" || got.Built != "2026-09-01T00:00:00Z" {
		t.Fatalf("stamp = %+v; the linker's values must not be second-guessed", got)
	}
}

// An unstamped build used to report "dev (none)" while carrying the
// commit it was built from all along. Anything the toolchain embedded
// is better than that.
func TestUnstampedBuildFallsBackToBuildInfo(t *testing.T) {
	t.Parallel()
	restore := stampFor(t, "dev", "none", "")
	defer restore()

	got := resolveBuildStamp()
	if got.Commit == "none" && got.Built == "" {
		t.Skip("this test binary carries no vcs metadata to fall back to")
	}
	if got.Commit == "none" {
		t.Errorf("commit stayed %q with vcs metadata available", got.Commit)
	}
}

// A pseudo-version encodes the commit and its timestamp, both already
// on the line. Only a real tag belongs in the version field.
func TestPseudoVersionsAreNotTreatedAsReleases(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"v0.0.0-20260901000653-8bfa854deeab", "(devel)", ""} {
		if isReleaseVersion(v) {
			t.Errorf("isReleaseVersion(%q) = true", v)
		}
	}
	for _, v := range []string{"v1.4.0", "v2.0.0-rc1"} {
		if !isReleaseVersion(v) {
			t.Errorf("isReleaseVersion(%q) = false; that is a tag", v)
		}
	}
}

// The rendered line is what --version prints and what an operator
// pastes into a bug report.
func TestStampRendersReadably(t *testing.T) {
	t.Parallel()
	b := buildStamp{Version: "v1.4.0", Commit: "abc123", Built: "2026-09-01T09:30:00Z"}
	if got := b.String(); got != "v1.4.0 (abc123, built 2026-09-01 09:30:00 UTC)" {
		t.Errorf("String() = %q", got)
	}

	b.Dirty = true
	if got := b.String(); !strings.Contains(got, "abc123-dirty") {
		t.Errorf("a dirty build must say so: %q", got)
	}
}

// An unparseable timestamp is still a timestamp worth printing.
func TestUnparseableBuildTimeSurvives(t *testing.T) {
	t.Parallel()
	if got := humanBuildTime("whenever"); got != "whenever" {
		t.Errorf("humanBuildTime dropped the value: %q", got)
	}
}

func stampFor(t *testing.T, version, commit, built string) func() {
	t.Helper()
	oldV, oldC, oldB := Version, Commit, BuildDate
	Version, Commit, BuildDate = version, commit, built
	return func() { Version, Commit, BuildDate = oldV, oldC, oldB }
}
