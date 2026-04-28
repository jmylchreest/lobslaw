package skills

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/internal/storage"
)

// fakeRunner records invocations and returns canned output.
type fakeRunner struct {
	mu     sync.Mutex
	argv   []string
	env    []string
	stdin  []byte
	policy *sandbox.Policy
	stdout string
	stderr string
	exit   int
	err    error
	runCnt int
}

func (f *fakeRunner) Run(_ context.Context, spec RunSpec) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runCnt++
	f.argv = append([]string(nil), spec.Argv...)
	f.env = append([]string(nil), spec.Env...)
	f.policy = spec.Policy
	if spec.Stdin != nil {
		b, _ := io.ReadAll(spec.Stdin)
		f.stdin = b
	}
	if f.stdout != "" {
		_, _ = spec.Stdout.Write([]byte(f.stdout))
	}
	if f.stderr != "" {
		_, _ = spec.Stderr.Write([]byte(f.stderr))
	}
	return f.exit, f.err
}

func TestInvokerRequiresRegistry(t *testing.T) {
	t.Parallel()
	_, err := NewInvoker(InvokerConfig{})
	if err == nil {
		t.Error("missing registry should fail")
	}
}

func TestInvokerMissingSkill(t *testing.T) {
	t.Parallel()
	inv, _ := NewInvoker(InvokerConfig{Registry: NewRegistry(nil), Runner: &fakeRunner{}})
	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "ghost"})
	if !errors.Is(err, ErrSkillNotFound) {
		t.Errorf("want ErrSkillNotFound; got %v", err)
	}
}

func TestInvokerRequiresBinaryMissing(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name:    "needs-gh",
			Version: "1.0.0",
			Runtime: RuntimeBash,
			Handler: "h.sh",
			RequiresBinary: []string{"gh"},
		},
		HandlerPath: "/mnt/skills/needs-gh/h.sh",
	})
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{
		Registry: reg,
		Runner:   runner,
		BinaryLookup: func(name string) (string, error) {
			return "", errors.New("not found")
		},
	})
	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "needs-gh"})
	if err == nil {
		t.Fatal("expected requires_binary failure")
	}
	if runner.runCnt != 0 {
		t.Errorf("runner should not be called when binary missing")
	}
	if !contains(err.Error(), "clawhub_install") {
		t.Errorf("error should suggest clawhub_install path: %v", err)
	}
}

func TestInvokerRequiresBinaryPresent(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name:    "needs-gh",
			Version: "1.0.0",
			Runtime: RuntimeBash,
			Handler: "h.sh",
			RequiresBinary: []string{"gh"},
		},
		HandlerPath: "/mnt/skills/needs-gh/h.sh",
	})
	runner := &fakeRunner{stdout: `{"ok":true}`}
	inv, _ := NewInvoker(InvokerConfig{
		Registry: reg,
		Runner:   runner,
		BinaryLookup: func(name string) (string, error) {
			return "/lobslaw/usr/bin/" + name, nil
		},
		BinaryInstallPrefix: "/lobslaw/usr",
	})
	if _, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "needs-gh"}); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if runner.runCnt != 1 {
		t.Errorf("runner should be invoked when binary present (got %d)", runner.runCnt)
	}
	pathEnvFound := false
	for _, e := range runner.env {
		if len(e) > 5 && e[:5] == "PATH=" {
			pathEnvFound = true
			if !contains(e, "/lobslaw/usr/bin") {
				t.Errorf("PATH should include /lobslaw/usr/bin: %s", e)
			}
		}
	}
	if !pathEnvFound {
		t.Errorf("PATH env should be injected when prefix is set; env=%v", runner.env)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestInvokerBashRuntimeArgv(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest:    Manifest{Name: "echo", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h.sh"},
		HandlerPath: "/mnt/skills/echo/h.sh",
	})
	runner := &fakeRunner{stdout: `{"ok":true}`}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner})

	res, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "echo", Params: map[string]any{"msg": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != `{"ok":true}` {
		t.Errorf("stdout: %q", res.Stdout)
	}
	if len(runner.argv) != 2 || runner.argv[0] != "bash" || runner.argv[1] != "/mnt/skills/echo/h.sh" {
		t.Errorf("argv: %v", runner.argv)
	}
}

func TestInvokerPythonRuntimeArgv(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest:    Manifest{Name: "py", Version: "1.0.0", Runtime: RuntimePython, Handler: "h.py"},
		HandlerPath: "/mnt/skills/py/h.py",
	})
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner})
	_, _ = inv.Invoke(context.Background(), InvokeRequest{SkillName: "py"})

	if runner.argv[0] != "python3" {
		t.Errorf("python runtime should launch python3; got %q", runner.argv[0])
	}
}

func TestInvokerPipesParamsAsJSON(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		HandlerPath: "/x/h",
	})
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner})

	_, err := inv.Invoke(context.Background(), InvokeRequest{
		SkillName: "s",
		Params:    map[string]any{"count": 42, "name": "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(runner.stdin, &got); err != nil {
		t.Fatalf("stdin not JSON: %s", runner.stdin)
	}
	// json numbers round-trip as float64
	if got["count"].(float64) != 42 || got["name"] != "alice" {
		t.Errorf("stdin params: %+v", got)
	}
}

// TestInvokerBuildsStorageEnv — each declared storage label
// becomes an LOBSLAW_STORAGE_<UPPER> env var pointing at the
// resolved path, so bash handlers can `cd $LOBSLAW_STORAGE_SHARED`.
func TestInvokerBuildsStorageEnv(t *testing.T) {
	t.Parallel()
	mgr := storage.NewManager()
	_ = mgr.Register(context.Background(), &fakeStorageMount{
		label: "shared", path: "/srv/shared",
	})
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Storage: []StorageAccess{{Label: "shared", Mode: StorageRead}},
		},
		HandlerPath: "/x/h",
	})
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner, Storage: mgr})

	_, _ = inv.Invoke(context.Background(), InvokeRequest{SkillName: "s"})

	found := ""
	for _, e := range runner.env {
		if e == "LOBSLAW_STORAGE_SHARED=/srv/shared" {
			found = e
		}
	}
	if found == "" {
		t.Errorf("expected LOBSLAW_STORAGE_SHARED in env; got %+v", runner.env)
	}
}

type stubCredentialIssuer struct {
	token   string
	scopes  []string
	expires time.Time
	err     error
	calls   []struct{ skill, provider, subject string }
}

func (s *stubCredentialIssuer) IssueForSkillByManifest(_ context.Context, skill, provider, subject string) (string, []string, time.Time, error) {
	s.calls = append(s.calls, struct{ skill, provider, subject string }{skill, provider, subject})
	if s.err != nil {
		return "", nil, time.Time{}, s.err
	}
	return s.token, s.scopes, s.expires, nil
}

func TestInvokerInjectsCredentialEnv(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name: "gws", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Credentials: []CredentialAccess{
				{Provider: "google", Subject: "alice@example.com"},
			},
		},
		HandlerPath: "/x/h",
	})
	stub := &stubCredentialIssuer{
		token:   "ya29.fresh",
		scopes:  []string{"gmail.readonly", "calendar.readonly"},
		expires: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{
		Registry:    reg,
		Runner:      runner,
		Credentials: stub,
	})

	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "gws"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.calls) != 1 {
		t.Fatalf("issuer should be called once; got %d", len(stub.calls))
	}
	want := struct{ skill, provider, subject string }{"gws", "google", "alice@example.com"}
	if stub.calls[0] != want {
		t.Errorf("issuer call = %+v; want %+v", stub.calls[0], want)
	}
	envSet := map[string]string{}
	for _, e := range runner.env {
		for i := range len(e) {
			if e[i] == '=' {
				envSet[e[:i]] = e[i+1:]
				break
			}
		}
	}
	if got := envSet["LOBSLAW_CRED_GOOGLE_TOKEN"]; got != "ya29.fresh" {
		t.Errorf("token env = %q", got)
	}
	if got := envSet["LOBSLAW_CRED_GOOGLE_SCOPES"]; got != "gmail.readonly calendar.readonly" {
		t.Errorf("scopes env = %q", got)
	}
	if got := envSet["LOBSLAW_CRED_GOOGLE_SUBJECT"]; got != "alice@example.com" {
		t.Errorf("subject env = %q", got)
	}
	if got := envSet["LOBSLAW_CRED_GOOGLE_EXPIRES"]; got != "2030-01-01T00:00:00Z" {
		t.Errorf("expires env = %q", got)
	}
}

func TestInvokerRejectsCredentialDeclWithoutIssuer(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name: "gws", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Credentials: []CredentialAccess{{Provider: "google"}},
		},
		HandlerPath: "/x/h",
	})
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: &fakeRunner{}})
	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "gws"})
	if err == nil {
		t.Error("credential declaration without an issuer should fail loudly")
	}
}

func TestInvokerSurfacesIssuerError(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name: "gws", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Credentials: []CredentialAccess{{Provider: "google"}},
		},
		HandlerPath: "/x/h",
	})
	stub := &stubCredentialIssuer{err: errors.New("not authorised")}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: &fakeRunner{}, Credentials: stub})
	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "gws"})
	if err == nil {
		t.Error("expected error to surface from issuer")
	}
}

// TestInvokerRejectsStorageAccessWithoutManager — a skill that
// declares storage access but is invoked without a configured
// Manager must fail loudly rather than silently dropping the
// declaration.
func TestInvokerRejectsStorageAccessWithoutManager(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Storage: []StorageAccess{{Label: "shared"}},
		},
		HandlerPath: "/x/h",
	})
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: &fakeRunner{}})
	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "s"})
	if err == nil {
		t.Error("storage access without manager should fail")
	}
}

// TestInvokerRejectsUnknownStorageLabel — declared labels must
// resolve at invocation time.
func TestInvokerRejectsUnknownStorageLabel(t *testing.T) {
	t.Parallel()
	mgr := storage.NewManager()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Storage: []StorageAccess{{Label: "not-registered"}},
		},
		HandlerPath: "/x/h",
	})
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: &fakeRunner{}, Storage: mgr})
	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "s"})
	if err == nil {
		t.Error("unknown storage label should fail")
	}
}

func TestInvokerCapturesExitCode(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		HandlerPath: "/x/h",
	})
	runner := &fakeRunner{exit: 42, stderr: "oops"}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner})
	res, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code: %d", res.ExitCode)
	}
	if string(res.Stderr) != "oops" {
		t.Errorf("stderr: %q", res.Stderr)
	}
}

func TestInvokerTimeoutApplies(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		HandlerPath: "/x/h",
	})
	// Runner that checks if its ctx is already cancelled — tests that
	// Invoke actually wires a timeout-bearing context through.
	var sawDeadline bool
	slowRunner := &deadlineRunner{observed: &sawDeadline}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: slowRunner})
	_, _ = inv.Invoke(context.Background(), InvokeRequest{
		SkillName: "s",
		Timeout:   50 * time.Millisecond,
	})
	if !sawDeadline {
		t.Error("runner didn't see a deadline; Invoke failed to propagate timeout")
	}
}

// deadlineRunner peeks at the ctx deadline to verify timeout
// propagation without actually waiting.
type deadlineRunner struct {
	observed *bool
}

func (d *deadlineRunner) Run(ctx context.Context, _ RunSpec) (int, error) {
	if _, ok := ctx.Deadline(); ok {
		*d.observed = true
	}
	return 0, nil
}

func TestPolicyEnablesNetworkFilterFromManifest(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest: Manifest{
			Name: "iso", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			NetworkIsolation: true,
			NetworkAllowDNS:  true,
		},
		HandlerPath: "/x/h",
	})
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner})
	if _, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "iso"}); err != nil {
		t.Fatal(err)
	}
	if runner.policy == nil {
		t.Fatal("policy not captured")
	}
	if !runner.policy.NetworkFilter {
		t.Error("NetworkFilter should be set when manifest opts in")
	}
	if !runner.policy.Namespaces.Network {
		t.Error("Namespaces.Network should be set when NetworkFilter requested")
	}
	if !runner.policy.NetworkAllowDNS {
		t.Error("NetworkAllowDNS should propagate from manifest")
	}
}

func TestPolicyDefaultsToNoNetworkIsolation(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest:    Manifest{Name: "x", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		HandlerPath: "/x/h",
	})
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner})
	_, _ = inv.Invoke(context.Background(), InvokeRequest{SkillName: "x"})
	if runner.policy.NetworkFilter {
		t.Error("NetworkFilter should default off")
	}
	if runner.policy.Namespaces.Network {
		t.Error("netns should default off")
	}
}

// fakeStorageMount for tests that need a resolvable label.
type fakeStorageMount struct {
	label string
	path  string
}

func (f *fakeStorageMount) Label() string                 { return f.label }
func (f *fakeStorageMount) Backend() string               { return "fake" }
func (f *fakeStorageMount) Path() string                  { return f.path }
func (f *fakeStorageMount) Start(_ context.Context) error { return nil }
func (f *fakeStorageMount) Stop(_ context.Context) error  { return nil }
func (f *fakeStorageMount) Healthy() bool                 { return true }

// --- Sandbox policy composition --------------------------------------

// TestPolicyIncludesManifestDirReadOnly — the handler directory
// must be readable by the interpreter but NOT writable. A skill
// modifying its own manifest would be confusing at best and
// tamper-with-audit at worst.
func findMount(mounts []sandbox.PolicyMount, path string) *sandbox.PolicyMount {
	for i := range mounts {
		if mounts[i].Path == path {
			return &mounts[i]
		}
	}
	return nil
}

func TestPolicyIncludesManifestDirReadExecOnly(t *testing.T) {
	t.Parallel()
	skill := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	p, err := buildPolicy(skill, "/bin/bash", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := findMount(p.Mounts, "/mnt/skills/s")
	if m == nil {
		t.Fatalf("manifest dir not in Mounts: %+v", p.Mounts)
	}
	if !m.Read || m.Write || !m.Exec {
		t.Errorf("manifest dir should be read+exec, not write: %+v", *m)
	}
}

func TestPolicyIncludesRuntimeDir(t *testing.T) {
	t.Parallel()
	skill := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	p, _ := buildPolicy(skill, "/usr/bin/bash", nil, nil)
	if findMount(p.Mounts, "/usr/bin") == nil {
		t.Errorf("runtime dir missing: %+v", p.Mounts)
	}
}

// TestPolicyStorageModeIntersectsMount — a skill declaring write
// against a writable mount keeps its write bit; a skill declaring
// read against an rx-mode mount inherits exec from the mount.
func TestPolicyStorageModeIntersectsMount(t *testing.T) {
	t.Parallel()
	mgr := storage.NewManager()
	_ = mgr.Register(context.Background(), &fakeStorageMount{label: "ro", path: "/srv/ro"})
	_ = mgr.Register(context.Background(), &fakeStorageMount{label: "rw", path: "/srv/rw"})

	skill := &Skill{
		Manifest: Manifest{
			Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Storage: []StorageAccess{
				{Label: "ro", Mode: StorageRead},
				{Label: "rw", Mode: StorageWrite},
			},
		},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	p, err := buildPolicy(skill, "/bin/bash", mgr, nil)
	if err != nil {
		t.Fatal(err)
	}
	ro := findMount(p.Mounts, "/srv/ro")
	if ro == nil || !ro.Read || ro.Write {
		t.Errorf("ro mount should be read-only: %+v", ro)
	}
	rw := findMount(p.Mounts, "/srv/rw")
	if rw == nil || !rw.Read || !rw.Write {
		t.Errorf("rw mount should be read+write: %+v", rw)
	}
}

// TestPolicyRejectsWriteOnReadOnlyMount — when a MountResolver is
// wired and the underlying mount mode forbids write, the skill must
// fail loudly at policy build time.
func TestPolicyRejectsWriteOnReadOnlyMount(t *testing.T) {
	t.Parallel()
	mgr := storage.NewManager()
	_ = mgr.Register(context.Background(), &fakeStorageMount{label: "config", path: "/etc/lobslaw"})

	skill := &Skill{
		Manifest: Manifest{
			Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Storage: []StorageAccess{{Label: "config", Mode: StorageWrite}},
		},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	resolver := &fakeMountResolver{
		known: map[string]struct {
			root    string
			r, w, x bool
		}{"config": {root: "/etc/lobslaw", r: true}},
	}
	if _, err := buildPolicy(skill, "/bin/bash", mgr, resolver); err == nil {
		t.Fatal("write on read-only mount should fail policy build")
	}
}

// TestPolicyStorageSubpathNarrowsLandlock — a skill declaring a
// subpath under a shared mount must end up with Landlock pointed at
// the narrowed directory, not the whole mount root. This is what
// lets multiple clawhub-installed skills co-exist under one
// operator-declared "skill-tools" / "skill-data" mount.
func TestPolicyStorageSubpathNarrowsLandlock(t *testing.T) {
	t.Parallel()
	mgr := storage.NewManager()
	_ = mgr.Register(context.Background(), &fakeStorageMount{label: "skill-tools", path: "/srv/skills"})

	skill := &Skill{
		Manifest: Manifest{
			Name: "gws-workspace", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h",
			Storage: []StorageAccess{
				{Label: "skill-tools", Subpath: "gws-workspace", Mode: StorageRead},
			},
		},
		ManifestDir: "/mnt/skills/gws-workspace",
		HandlerPath: "/mnt/skills/gws-workspace/h",
	}
	resolver := &fakeMountResolver{
		known: map[string]struct {
			root    string
			r, w, x bool
		}{"skill-tools": {root: "/srv/skills", r: true, x: true}},
	}
	p, err := buildPolicy(skill, "/bin/bash", mgr, resolver)
	if err != nil {
		t.Fatalf("buildPolicy: %v", err)
	}
	want := "/srv/skills/gws-workspace"
	got := findMount(p.Mounts, want)
	if got == nil {
		t.Fatalf("expected mount at %q in policy: %+v", want, p.Mounts)
	}
	// Sibling skill must NOT see the broader root in its allowlist.
	if findMount(p.Mounts, "/srv/skills") != nil {
		t.Errorf("skill should NOT see the unsubpath'd root /srv/skills")
	}
}

// TestSubpathRejectsTraversal — manifest validation must catch
// "../etc" before the resolver ever sees it.
func TestSubpathRejectsTraversal(t *testing.T) {
	t.Parallel()
	cases := []string{"../etc", "/abs/path", "foo/../../etc", ".."}
	for _, sp := range cases {
		t.Run(sp, func(t *testing.T) {
			m := Manifest{
				Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h.sh",
				Storage: []StorageAccess{{Label: "skill-tools", Subpath: sp, Mode: StorageRead}},
			}
			tmp := t.TempDir()
			if err := os.WriteFile(filepath.Join(tmp, "h.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := validateManifest(&m, tmp); err == nil {
				t.Errorf("subpath %q should be rejected by manifest validation", sp)
			}
		})
	}
}

type fakeMountResolver struct {
	known map[string]struct {
		root    string
		r, w, x bool
	}
}

func (f *fakeMountResolver) ModeForLabel(label string) (string, bool, bool, bool, bool) {
	m, ok := f.known[label]
	if !ok {
		return "", false, false, false, false
	}
	return m.root, m.r, m.w, m.x, true
}

// TestPolicyEnforcesNoNewPrivs — any skill invocation runs with
// PR_SET_NO_NEW_PRIVS. Blocks setuid binary escalation in an
// over-permissive storage mount (e.g. operator accidentally
// bind-mounting /bin).
func TestPolicyEnforcesNoNewPrivs(t *testing.T) {
	t.Parallel()
	skill := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	p, _ := buildPolicy(skill, "/bin/bash", nil, nil)
	if !p.NoNewPrivs {
		t.Error("skills must run with NoNewPrivs")
	}
}

// TestPolicyUserNamespaceEnabled — user namespace is the gate
// that makes unprivileged operation of the rest of the namespaces
// possible. Required, not optional.
func TestPolicyUserNamespaceEnabled(t *testing.T) {
	t.Parallel()
	skill := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	p, _ := buildPolicy(skill, "/bin/bash", nil, nil)
	if !p.Namespaces.User {
		t.Error("user namespace must be enabled for skills")
	}
}

// TestInvokerPassesPolicyToRunner — the fakeRunner captures the
// policy so we can assert the full integration: Invoke →
// buildPolicy → runner.Run(RunSpec{Policy: ...}).
func TestInvokerPassesPolicyToRunner(t *testing.T) {
	t.Parallel()
	reg := NewRegistry(nil)
	reg.Put(&Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	})
	runner := &fakeRunner{}
	inv, _ := NewInvoker(InvokerConfig{Registry: reg, Runner: runner})
	_, err := inv.Invoke(context.Background(), InvokeRequest{SkillName: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if runner.policy == nil {
		t.Fatal("policy not propagated to runner")
	}
	if !runner.policy.NoNewPrivs {
		t.Error("NoNewPrivs should be on in the runner-observed policy")
	}
}

func containsString(s []string, needle string) bool {
	for _, v := range s {
		if v == needle {
			return true
		}
	}
	return false
}

// Compile-time guard that the sandbox import is actually used.
var _ = sandbox.Policy{}
