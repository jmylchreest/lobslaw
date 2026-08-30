//go:build linux

package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/landlock-lsm/go-landlock/landlock"
	llsyscall "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// Landlock restricts the whole process and cannot be undone, so every
// enforcement assertion runs in a child: this test binary re-execs
// itself, the child installs the policy and reports what it could
// reach, and the parent reads the verdict off stdout.
const (
	landlockChildEnv = "LOBSLAW_TEST_LANDLOCK_POLICY"
	landlockProbeEnv = "LOBSLAW_TEST_LANDLOCK_PROBE"
)

// TestLandlockChild is the child half of the re-exec. It is a Test
// function because that is how the test binary lets us re-enter it;
// it asserts nothing itself and skips when run as part of the
// ordinary suite.
func TestLandlockChild(t *testing.T) {
	spec := os.Getenv(landlockChildEnv)
	if spec == "" {
		t.Skip("child half of the landlock re-exec; driven by its parent")
	}
	var p Policy
	if err := json.Unmarshal([]byte(spec), &p); err != nil {
		reportAndExit("BAD-SPEC: " + err.Error())
	}
	if err := installLandlock(&p); err != nil {
		reportAndExit("INSTALL-FAILED: " + err.Error())
	}
	probe := os.Getenv(landlockProbeEnv)
	f, err := os.OpenFile(probe, os.O_WRONLY, 0)
	if err != nil {
		reportAndExit("WRITE-DENIED: " + err.Error())
	}
	_ = f.Close()
	reportAndExit("WRITE-OK")
}

// reportAndExit writes the verdict and leaves immediately, so the
// testing framework's own output never lands on the same stream the
// parent is parsing.
func reportAndExit(verdict string) {
	_, _ = os.Stdout.WriteString(verdict + "\n")
	os.Exit(0)
}

// runLandlockChild re-execs this binary, installs p in the child, and
// returns the child's verdict for opening probe for writing.
func runLandlockChild(t *testing.T, p Policy, probe string) string {
	t.Helper()
	if abi, err := llsyscall.LandlockGetABIVersion(); err != nil || abi < 1 {
		t.Skipf("kernel has no Landlock support (abi=%d err=%v)", abi, err)
	}
	spec, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal policy: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLandlockChild$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		landlockChildEnv+"="+string(spec),
		landlockProbeEnv+"="+probe,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child failed: %v\noutput:\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// The bug this whole change exists for: no policy anywhere granted
// /dev, so `cmd 2>/dev/null` — which is ordinary shell, not a
// privileged act — died on EACCES.
func TestADeviceInThePolicyIsActuallyWritable(t *testing.T) {
	t.Parallel()
	got := runLandlockChild(t, Policy{
		Mounts: []PolicyMount{
			{Path: "/dev/null", Read: true, Write: true},
		},
	}, "/dev/null")
	if got != "WRITE-OK" {
		t.Errorf("child verdict = %q, want WRITE-OK — /dev/null was granted", got)
	}
}

// And the converse, so the test above is proving a grant rather than
// an absent sandbox: a policy that doesn't name /dev/null must deny it.
func TestADeviceOutsideThePolicyIsDenied(t *testing.T) {
	t.Parallel()
	got := runLandlockChild(t, Policy{
		Mounts: []PolicyMount{
			{Path: "/tmp", Read: true, Write: true},
		},
	}, "/dev/null")
	if !strings.HasPrefix(got, "WRITE-DENIED") {
		t.Errorf("child verdict = %q, want WRITE-DENIED — /dev/null was not granted", got)
	}
}

// A file entry used to take the entire install down with it:
// landlock_add_rule returns EINVAL for directory rights on a
// non-directory, and IgnoreIfMissing only covers ENOENT. So a policy
// naming one file lost every path in it, not just that one — which is
// how the `dns` and `git-config` presets would have behaved.
func TestAFileEntryDoesNotTakeTheRestOfThePolicyDownWithIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := dir + "/writable"
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runLandlockChild(t, Policy{
		Mounts: []PolicyMount{
			{Path: "/etc/hosts", Read: true},
			{Path: dir, Read: true, Write: true},
		},
	}, target)
	if got != "WRITE-OK" {
		t.Errorf("child verdict = %q, want WRITE-OK — the file entry must not void the directory entry", got)
	}
}

// Same hazard through the legacy fields, which is the shape the
// policy.d loader produces.
func TestAFileInAllowedPathsDoesNotVoidThePolicy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := dir + "/writable"
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := runLandlockChild(t, Policy{
		AllowedPaths:  []string{"/etc/hosts", dir},
		ReadOnlyPaths: []string{"/etc/hosts"},
	}, target)
	if got != "WRITE-OK" {
		t.Errorf("child verdict = %q, want WRITE-OK", got)
	}
}

// The legacy pair was installed through landlock.RODirs / RWDirs,
// both of which grant execute, so "read-only" in that vocabulary has
// always meant read+execute. effectiveMounts is the single place that
// conversion happens — the Landlock install and the in-process
// AllowsPath evaluator both read it, so they cannot disagree about a
// path the operator wrote down once.
func TestLegacyPathsBecomeMountsWithTheRightsRODirsGranted(t *testing.T) {
	t.Parallel()
	p := &Policy{
		AllowedPaths:  []string{"/ro", "/rw"},
		ReadOnlyPaths: []string{"/ro"},
	}
	got := MergeMounts(p.effectiveMounts())
	want := []PolicyMount{
		{Path: "/ro", Read: true, Exec: true},
		{Path: "/rw", Read: true, Exec: true, Write: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("effectiveMounts() = %+v, want %+v", got, want)
	}
}

func TestAPolicyWithNoPathsHasNoMounts(t *testing.T) {
	t.Parallel()
	p := &Policy{ReadOnlyPaths: []string{"/ro"}}
	if got := p.effectiveMounts(); len(got) != 0 {
		t.Errorf("effectiveMounts() = %+v, want empty", got)
	}
}

func TestAccessIsNarrowedForFilesAndLeftAloneForDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := dir + "/f"
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	full := mountModeAccessFS(PolicyMount{Path: file, Read: true, Write: true, Exec: true})

	if gotDir := accessForPathKind(dir, full); gotDir != full {
		t.Errorf("directory access = %v, want it left alone (%v)", gotDir, full)
	}

	gotFile := accessForPathKind(file, full)
	if gotFile == full {
		t.Error("file access was not narrowed; directory-only rights make landlock_add_rule EINVAL")
	}
	if gotFile&landlock.AccessFSSet(llsyscall.AccessFSMakeReg) != 0 {
		t.Error("file access still carries a directory-only right")
	}
	if gotFile&landlock.AccessFSSet(llsyscall.AccessFSWriteFile) == 0 {
		t.Error("narrowing dropped write access the caller asked for")
	}
}

// The ioctl grant is an allow-list, not "any device in the policy":
// device ioctls are where the interesting privileges live, so granting
// them wherever an operator wrote `:r` would widen the policy past
// what its author asked for.
func TestOnlyKnownHarmlessDevicesAreGrantedIoctl(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Skip("no /dev/null on this system")
	}
	if !ioctlSafeDevice("/dev/null") {
		t.Error("/dev/null was not recognised; isatty() on it would fail with EACCES")
	}
	for _, path := range []string{"/dev/sda", "/dev/net/tun", "/dev/kvm", "/dev/loop-control"} {
		if ioctlSafeDevice(path) {
			t.Errorf("%s would be granted ioctl; the allow-list must not cover it", path)
		}
	}
}

// The name alone is not enough — a policy could name /dev/null on a
// system where something else lives at that path.
func TestAnOrdinaryFileNamedLikeADeviceIsNotGrantedIoctl(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := os.Create(dir + "/null"); err != nil {
		t.Fatal(err)
	}
	if ioctlSafeDevice(dir + "/null") {
		t.Error("a regular file was treated as a device")
	}
}

// A missing path is left at full access rather than narrowed on a
// guess — IgnoreIfMissing drops it at install time either way.
func TestAMissingPathIsLeftForIgnoreIfMissingToDrop(t *testing.T) {
	t.Parallel()
	full := mountModeAccessFS(PolicyMount{Path: "/nope", Read: true, Write: true})
	if got := accessForPathKind("/no/such/path/anywhere", full); got != full {
		t.Errorf("missing path access = %v, want %v", got, full)
	}
}
