package skills

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
func TestPolicyIncludesManifestDirReadOnly(t *testing.T) {
	t.Parallel()
	skill := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	p, err := buildPolicy(skill, "/bin/bash", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(p.AllowedPaths, "/mnt/skills/s") {
		t.Errorf("manifest dir not in AllowedPaths: %+v", p.AllowedPaths)
	}
	if !containsString(p.ReadOnlyPaths, "/mnt/skills/s") {
		t.Errorf("manifest dir should be read-only: %+v", p.ReadOnlyPaths)
	}
}

// TestPolicyIncludesRuntimeDir — the interpreter binary's dir must
// be readable so python3 can load its stdlib / bash can locate
// helpers. Full /usr would be overbroad; we settle for the
// interpreter's immediate directory.
func TestPolicyIncludesRuntimeDir(t *testing.T) {
	t.Parallel()
	skill := &Skill{
		Manifest:    Manifest{Name: "s", Version: "1.0.0", Runtime: RuntimeBash, Handler: "h"},
		ManifestDir: "/mnt/skills/s",
		HandlerPath: "/mnt/skills/s/h",
	}
	p, _ := buildPolicy(skill, "/usr/bin/bash", nil)
	if !containsString(p.AllowedPaths, "/usr/bin") {
		t.Errorf("runtime dir missing: %+v", p.AllowedPaths)
	}
}

// TestPolicyStorageReadOnlyRespected — a skill declaring read-only
// access ends up with that mount's path in ReadOnlyPaths; write
// mode stays rw.
func TestPolicyStorageReadOnlyRespected(t *testing.T) {
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
	p, err := buildPolicy(skill, "/bin/bash", mgr)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(p.AllowedPaths, "/srv/ro") || !containsString(p.ReadOnlyPaths, "/srv/ro") {
		t.Errorf("ro mount should be rw'd + read-only: %+v", p)
	}
	if !containsString(p.AllowedPaths, "/srv/rw") {
		t.Errorf("rw mount should be in AllowedPaths: %+v", p.AllowedPaths)
	}
	if containsString(p.ReadOnlyPaths, "/srv/rw") {
		t.Errorf("rw mount MUST NOT be read-only: %+v", p.ReadOnlyPaths)
	}
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
	p, _ := buildPolicy(skill, "/bin/bash", nil)
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
	p, _ := buildPolicy(skill, "/bin/bash", nil)
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
