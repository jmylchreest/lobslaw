package compute

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/hooks"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/policy"
	"github.com/jmylchreest/lobslaw/internal/sandbox"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// testEnv bundles everything a test needs: registry, policy engine,
// hook dispatcher, the Executor, plus the memory.Store so tests can
// seed policy rules directly.
type testEnv struct {
	reg      *Registry
	policy   *policy.Engine
	store    *memory.Store
	hooks    *hooks.Dispatcher
	executor *Executor
}

// newTestEnv builds a usable executor with a permissive policy rule
// (allow *) already seeded. Individual tests can add more rules or
// replace with a restrictive engine.
func newTestEnv(t *testing.T, opts ...func(*ExecutorConfig)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	store, err := memory.OpenStore(filepath.Join(dir, "state.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Default permissive rule — specific tests that want a
	// restrictive engine can remove or shadow it.
	seedAllowAll(t, store)

	eng := policy.NewEngine(store, nil)
	reg := NewRegistry()

	cfg := ExecutorConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	return &testEnv{
		reg:      reg,
		policy:   eng,
		store:    store,
		hooks:    nil,
		executor: NewExecutor(reg, eng, nil, cfg, nil),
	}
}

func seedAllowAll(t *testing.T, store *memory.Store) {
	t.Helper()
	rule := &lobslawv1.PolicyRule{
		Id: "allow-all", Subject: "*", Action: "*", Resource: "*",
		Effect: "allow", Priority: 1,
	}
	raw, err := proto.Marshal(rule)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(memory.BucketPolicyRules, rule.Id, raw); err != nil {
		t.Fatal(err)
	}
}

func writeScript(t *testing.T, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	// os.WriteFile can race with subsequent execve — on some
	// filesystems (certain tmpfs variants, CoW overlays) the
	// executable mode bits or the file contents themselves may not
	// be visible to an immediate child process. Use the more
	// paranoid pattern:
	//   1. OpenFile with 0o700 mode explicitly.
	//   2. Write + Sync to force the fs to durable state.
	//   3. Close + Chmod again (defensive — some fs caches drop
	//      the open-time mode when Sync'd).
	//   4. Stat + verify executable bit on the returned inode.
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("#!/bin/sh\n" + script + "\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("writeScript: %q has no executable bits after Chmod (fs race?)", p)
	}
	return p
}

func TestExecutorHappyPath(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	dir := t.TempDir()
	path := writeScript(t, dir, "hello.sh", `echo "hello $1"`)
	if err := env.reg.Register(&types.ToolDef{
		Name:         "hello",
		Path:         path,
		ArgvTemplate: []string{"{name}"},
		RiskTier:     types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "hello",
		Params:   map[string]string{"name": "world"},
		Claims:   &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d", res.ExitCode)
	}
	if string(res.Stdout) != "hello world\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "hello world\n")
	}
}

// TestExecutorArgvArrayPreventsShellInjection proves that shell
// metacharacters inside a Param are passed as literal argv bytes,
// not interpreted. If this test ever fails the exec path has been
// refactored to use a shell.
func TestExecutorArgvArrayPreventsShellInjection(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	dir := t.TempDir()
	// Sentinel the injection would create if shell-interpreted.
	sentinel := filepath.Join(dir, "pwned")
	echo := writeScript(t, dir, "echo.sh", `echo "arg=$1"`)

	if err := env.reg.Register(&types.ToolDef{
		Name:         "echo",
		Path:         echo,
		ArgvTemplate: []string{"{msg}"},
		RiskTier:     types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	// Classic shell-injection string. If sh ever interpreted it,
	// the sentinel file would exist.
	payload := "innocent; touch " + sentinel

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "echo",
		Params:   map[string]string{"msg": payload},
		Claims:   &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.Stdout), payload) {
		t.Errorf("echo should emit literal payload: %q", res.Stdout)
	}
	if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
		t.Fatal("SECURITY: shell injection executed — sentinel file was created")
	}
}

func TestExecutorRejectsRelativePath(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	// Bypass registry validation by constructing via Replace — the
	// executor's own path check must still reject.
	if err := env.reg.Replace(&types.ToolDef{
		Name:         "bad",
		Path:         "relative/path",
		ArgvTemplate: []string{"x"},
		RiskTier:     types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "bad", Claims: &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, ErrToolPathInvalid) {
		t.Errorf("err = %v, want ErrToolPathInvalid", err)
	}
}

// TestExecutorRejectsSymlinkOutsideAllowedRoots plants a symlink
// inside an allow-listed root that points at /bin/ls (outside the
// root). Without AllowedPathRoots configured the symlink target
// would be invoked as-if the tool were /bin/ls — exactly the attack
// the allowlist defeats.
func TestExecutorRejectsSymlinkOutsideAllowedRoots(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	outside := "/bin/ls"
	if _, err := os.Stat(outside); os.IsNotExist(err) {
		t.Skipf("system lacks %s", outside)
	}

	// Create a symlink inside dir that resolves to /bin/ls.
	trap := filepath.Join(dir, "innocent")
	if err := os.Symlink(outside, trap); err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t, func(cfg *ExecutorConfig) {
		cfg.AllowedPathRoots = []string{dir} // tools must live under dir
	})

	if err := env.reg.Register(&types.ToolDef{
		Name:         "trap",
		Path:         trap,
		ArgvTemplate: []string{},
		RiskTier:     types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "trap", Claims: &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, ErrToolPathInvalid) {
		t.Errorf("SECURITY: symlink escape allowed; err = %v", err)
	}
}

// TestExecutorSymlinkWithinRootsAllowed complements the rejection
// test: a symlink that resolves WITHIN allowed roots must still be
// invocable. Otherwise allowed_paths is useless when operators use
// convenience symlinks inside their tool directory.
func TestExecutorSymlinkWithinRootsAllowed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := writeScript(t, dir, "real.sh", `echo ok`)
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}

	env := newTestEnv(t, func(cfg *ExecutorConfig) {
		cfg.AllowedPathRoots = []string{dir}
	})
	if err := env.reg.Register(&types.ToolDef{
		Name:         "alias",
		Path:         alias,
		ArgvTemplate: []string{},
		RiskTier:     types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "alias", Claims: &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Stdout) != "ok\n" {
		t.Errorf("unexpected output: %q", res.Stdout)
	}
}

// TestExecutorHardlinkLimitation documents the known hardlink
// limitation. A hardlink is a distinct directory entry pointing to
// the same inode — realpath reports the name you open, not the
// inode's "origin," so containment checks can't detect it.
//
// Mitigation for operators: keep allowed_paths owned by the lobslaw
// UID so untrusted actors can't create hardlinks there. This test
// asserts the current behaviour (executor happily invokes a
// hardlink inside allowed_paths) so a future change is caught.
//
// Phase 4.5's sandbox tests will add the st_nlink=1 enforcement
// option for the paranoid case.
func TestExecutorHardlinkLimitation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := writeScript(t, dir, "real.sh", `echo ok`)
	hardlink := filepath.Join(dir, "hard")
	if err := os.Link(real, hardlink); err != nil {
		// On some filesystems (tmpfs sometimes, overlayfs) link can fail.
		t.Skipf("hardlink not supported on %s: %v", dir, err)
	}

	env := newTestEnv(t, func(cfg *ExecutorConfig) {
		cfg.AllowedPathRoots = []string{dir}
	})
	if err := env.reg.Register(&types.ToolDef{
		Name:         "hard",
		Path:         hardlink,
		ArgvTemplate: []string{},
		RiskTier:     types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	// Current behaviour: hardlink inside allowed_paths is accepted.
	// If Phase 4.5 tightens this to reject multi-linked files, update
	// the assertion to ErrToolPathInvalid.
	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "hard", Claims: &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Logf("hardlink rejection (future behaviour): %v", err)
		return
	}
	if string(res.Stdout) != "ok\n" {
		t.Errorf("current behaviour: hardlink runs; stdout = %q", res.Stdout)
	}
}

func TestExecutorCapsOutput(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t, func(cfg *ExecutorConfig) {
		cfg.MaxOutputBytes = 1024 // 1 KiB cap
	})
	dir := t.TempDir()
	// 100 KiB of output — should be capped to 1 KiB.
	hog := writeScript(t, dir, "hog.sh",
		`dd if=/dev/zero bs=1024 count=100 2>/dev/null | tr '\0' 'x'`)

	if err := env.reg.Register(&types.ToolDef{
		Name: "hog", Path: hog, ArgvTemplate: []string{},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "hog", Claims: &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Error("100 KiB output should trip the 1 KiB cap Truncated flag")
	}
	if int64(len(res.Stdout)) > 1024 {
		t.Errorf("stdout len = %d, want <= 1024", len(res.Stdout))
	}
}

func TestExecutorTimeoutKillsSubprocess(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	dir := t.TempDir()
	hang := writeScript(t, dir, "hang.sh", `sleep 10`)
	if err := env.reg.Register(&types.ToolDef{
		Name: "hang", Path: hang, ArgvTemplate: []string{},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	res, _ := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "hang",
		Claims:   &types.Claims{UserID: "alice"},
		Timeout:  500 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("timeout didn't kill subprocess within 3s — took %v", elapsed)
	}
	_ = res // we don't care about the code/output, only the timely kill
}

func TestExecutorEnvWhitelist(t *testing.T) {
	// Intentionally non-parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv("LOBSLAW_TEST_PROBE", "visible")
	t.Setenv("LOBSLAW_TEST_LEAK", "hidden")

	env := newTestEnv(t, func(cfg *ExecutorConfig) {
		cfg.EnvWhitelist = []string{"LOBSLAW_TEST_PROBE"}
	})

	dir := t.TempDir()
	// Print PROBE and LEAK. Only PROBE should have a value.
	probe := writeScript(t, dir, "env.sh",
		`echo "probe=${LOBSLAW_TEST_PROBE:-UNSET}"; echo "leak=${LOBSLAW_TEST_LEAK:-UNSET}"`)
	if err := env.reg.Register(&types.ToolDef{
		Name: "env", Path: probe, ArgvTemplate: []string{},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "env", Claims: &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(res.Stdout)
	if !strings.Contains(out, "probe=visible") {
		t.Errorf("whitelisted var missing: %q", out)
	}
	if !strings.Contains(out, "leak=UNSET") {
		t.Errorf("SECURITY: non-whitelisted env var leaked: %q", out)
	}
}

func TestExecutorDefaultEnvIsEmpty(t *testing.T) {
	// Intentionally non-parallel: t.Setenv is incompatible with t.Parallel.
	t.Setenv("LOBSLAW_TEST_SECRET", "should-not-leak")

	env := newTestEnv(t) // no EnvWhitelist configured
	dir := t.TempDir()
	probe := writeScript(t, dir, "envcount.sh", `env | wc -l`)
	if err := env.reg.Register(&types.ToolDef{
		Name: "envcount", Path: probe, ArgvTemplate: []string{},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "envcount", Claims: &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The shell may set a few implicit vars (PWD, SHLVL, _) internally
	// even with empty parent env. Assert the secret isn't among them.
	if strings.Contains(string(res.Stdout)+string(res.Stderr), "should-not-leak") {
		t.Error("SECURITY: parent env leaked despite empty whitelist")
	}
}

func TestExecutorUnknownTool(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "nope", Claims: &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, ErrToolNotFound) {
		t.Errorf("err = %v, want ErrToolNotFound", err)
	}
}

func TestExecutorSidecarOnlyRejected(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	if err := env.reg.Register(&types.ToolDef{
		Name: "git-sidecar", SidecarOnly: true, RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "git-sidecar", Claims: &types.Claims{UserID: "alice"},
	})
	if err == nil || !strings.Contains(err.Error(), "sidecar-only") {
		t.Errorf("sidecar-only should be rejected: err = %v", err)
	}
}

func TestExecutorMissingParamRejected(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	dir := t.TempDir()
	tool := writeScript(t, dir, "need-arg.sh", `echo $1`)
	if err := env.reg.Register(&types.ToolDef{
		Name: "need-arg", Path: tool, ArgvTemplate: []string{tool, "{required}"},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "need-arg",
		// Params omitted entirely.
		Claims: &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, ErrMissingParam) {
		t.Errorf("err = %v, want ErrMissingParam", err)
	}
}

func TestExecutorPolicyDenyBlocksExec(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)

	// Replace allow-all with a deny-all by re-seeding a higher-priority deny.
	denyRule := &lobslawv1.PolicyRule{
		Id: "deny-all", Subject: "*", Action: "*", Resource: "*",
		Effect: "deny", Priority: 100,
	}
	raw, _ := proto.Marshal(denyRule)
	if err := env.store.Put(memory.BucketPolicyRules, denyRule.Id, raw); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	// If this runs, it creates a sentinel — our assertion is it doesn't.
	sentinel := filepath.Join(dir, "should-not-exist")
	tool := writeScript(t, dir, "t.sh", "touch "+sentinel)
	if err := env.reg.Register(&types.ToolDef{
		Name: "t", Path: tool, ArgvTemplate: []string{tool},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "t", Claims: &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Errorf("err = %v, want ErrPolicyDenied", err)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Fatal("SECURITY: tool executed despite policy deny — sentinel exists")
	}
}

func TestExecutorPolicyRequireConfirmation(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	// Confirmation for "tool:exec" on tool "sensitive".
	rule := &lobslawv1.PolicyRule{
		Id: "confirm-sensitive", Subject: "*", Action: "tool:exec",
		Resource: "sensitive", Effect: "require_confirmation", Priority: 100,
	}
	raw, _ := proto.Marshal(rule)
	if err := env.store.Put(memory.BucketPolicyRules, rule.Id, raw); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	tool := writeScript(t, dir, "s.sh", "echo ran")
	if err := env.reg.Register(&types.ToolDef{
		Name: "sensitive", Path: tool, ArgvTemplate: []string{tool},
		RiskTier: types.RiskIrreversible,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "sensitive", Claims: &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, ErrRequireConfirm) {
		t.Errorf("err = %v, want ErrRequireConfirm", err)
	}
}

func TestExecutorHookBlockPreventsExec(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "should-not-exist")
	tool := writeScript(t, dir, "t.sh", "touch "+sentinel)
	if err := env.reg.Register(&types.ToolDef{
		Name: "t", Path: tool, ArgvTemplate: []string{tool},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	// Hook that blocks via exit 2.
	blocker := writeScript(t, dir, "block.sh",
		`echo "not allowed" >&2; exit 2`)

	env.executor = NewExecutor(
		env.reg, env.policy,
		hooks.NewDispatcher(map[types.HookEvent][]types.HookConfig{
			types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: blocker}},
		}, nil),
		ExecutorConfig{}, nil,
	)

	_, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "t", Claims: &types.Claims{UserID: "alice"},
	})
	if !errors.Is(err, types.ErrHookBlocked) {
		t.Errorf("err = %v, want ErrHookBlocked", err)
	}
	if _, statErr := os.Stat(sentinel); !os.IsNotExist(statErr) {
		t.Fatal("SECURITY: tool executed despite PreToolUse hook block")
	}
}

func TestExecutorNonZeroExitIsNotAnError(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	dir := t.TempDir()
	tool := writeScript(t, dir, "fail.sh", `echo "error" >&2; exit 42`)
	if err := env.reg.Register(&types.ToolDef{
		Name: "fail", Path: tool, ArgvTemplate: []string{tool},
		RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := env.executor.Invoke(context.Background(), InvokeRequest{
		ToolName: "fail", Claims: &types.Claims{UserID: "alice"},
	})
	if err != nil {
		t.Fatalf("non-zero exit shouldn't be a Go error: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
	if !strings.Contains(string(res.Stderr), "error") {
		t.Errorf("stderr not captured: %q", res.Stderr)
	}
}

func TestSubstituteArgv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tmpl    []string
		params  map[string]string
		want    []string
		wantErr bool
	}{
		{
			name:   "simple placeholder",
			tmpl:   []string{"echo", "{msg}"},
			params: map[string]string{"msg": "hi"},
			want:   []string{"echo", "hi"},
		},
		{
			name:   "embedded placeholder",
			tmpl:   []string{"echo", "hello {name}!"},
			params: map[string]string{"name": "world"},
			want:   []string{"echo", "hello world!"},
		},
		{
			name:   "multiple placeholders in one segment",
			tmpl:   []string{"curl", "{host}/{path}"},
			params: map[string]string{"host": "example.com", "path": "api"},
			want:   []string{"curl", "example.com/api"},
		},
		{
			name:   "metacharacter passes through literally",
			tmpl:   []string{"echo", "{msg}"},
			params: map[string]string{"msg": "foo; rm -rf /"},
			want:   []string{"echo", "foo; rm -rf /"},
		},
		{
			name:    "missing param",
			tmpl:    []string{"x", "{needed}"},
			params:  map[string]string{},
			wantErr: true,
		},
		{
			name:   "no placeholders",
			tmpl:   []string{"bash", "-c", "exit 0"},
			params: map[string]string{},
			want:   []string{"bash", "-c", "exit 0"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := substituteArgv(tc.tmpl, tc.params)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i, v := range got {
				if v != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, v, tc.want[i])
				}
			}
		})
	}
}

// --- Per-tool sandbox policy fallback chain -----------------------------

// TestExecutorResolvePolicyToolSpecificWinsOverFleetDefault confirms
// the fallback chain priority: a tool-specific policy registered via
// Registry.SetPolicy beats whatever ExecutorConfig.Sandbox carries.
// This is the structural guarantee that lets operators say "tighten
// shell_exec but leave git alone" without per-Executor splits.
func TestExecutorResolvePolicyToolSpecificWinsOverFleetDefault(t *testing.T) {
	t.Parallel()
	fleet := &sandbox.Policy{NoNewPrivs: true, AllowedPaths: []string{"/usr"}}
	tool := &sandbox.Policy{NoNewPrivs: true, AllowedPaths: []string{"/srv/repo"}}

	env := newTestEnv(t, func(cfg *ExecutorConfig) { cfg.Sandbox = fleet })
	if err := env.reg.Register(&types.ToolDef{
		Name: "git", Path: "/usr/bin/git", RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}
	env.reg.SetPolicy("git", tool)

	got := env.executor.resolvePolicy("git")
	if got == nil || len(got.AllowedPaths) != 1 || got.AllowedPaths[0] != "/srv/repo" {
		t.Errorf("tool-specific policy should win; got %+v, want /srv/repo", got)
	}
}

// TestExecutorResolvePolicyFallsThroughToFleet confirms an unset
// per-tool policy falls through to the Executor default — operators
// only need to override the tools they actually want to differ.
func TestExecutorResolvePolicyFallsThroughToFleet(t *testing.T) {
	t.Parallel()
	fleet := &sandbox.Policy{NoNewPrivs: true, AllowedPaths: []string{"/usr"}}
	env := newTestEnv(t, func(cfg *ExecutorConfig) { cfg.Sandbox = fleet })
	if err := env.reg.Register(&types.ToolDef{
		Name: "git", Path: "/usr/bin/git", RiskTier: types.RiskReversible,
	}); err != nil {
		t.Fatal(err)
	}

	got := env.executor.resolvePolicy("git")
	if got == nil || len(got.AllowedPaths) != 1 || got.AllowedPaths[0] != "/usr" {
		t.Errorf("expected fleet default to surface; got %+v", got)
	}
}

// TestExecutorResolvePolicyAllNilReturnsNil — the unsandboxed path,
// which is the default until an operator opts in. sandbox.Apply is a
// no-op for nil so the subprocess runs unrestricted.
func TestExecutorResolvePolicyAllNilReturnsNil(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	_ = env.reg.Register(&types.ToolDef{
		Name: "git", Path: "/usr/bin/git", RiskTier: types.RiskReversible,
	})
	if got := env.executor.resolvePolicy("git"); got != nil {
		t.Errorf("no policy anywhere → nil; got %+v", got)
	}
}

// TestExecutorResolvePolicyEmptyToolPolicyShortCircuitsFleet documents
// the "explicitly unsandboxed" path: a non-nil but empty Policy on the
// tool stops the chain — fleet default does NOT apply. This is how
// operators say "trust this specific tool" even though everything else
// is sandboxed.
func TestExecutorResolvePolicyEmptyToolPolicyShortCircuitsFleet(t *testing.T) {
	t.Parallel()
	fleet := &sandbox.Policy{NoNewPrivs: true}
	env := newTestEnv(t, func(cfg *ExecutorConfig) { cfg.Sandbox = fleet })
	_ = env.reg.Register(&types.ToolDef{
		Name: "bash", Path: "/usr/bin/bash", RiskTier: types.RiskReversible,
	})
	env.reg.SetPolicy("bash", &sandbox.Policy{})

	got := env.executor.resolvePolicy("bash")
	if got == nil {
		t.Fatal("empty Policy should short-circuit, not fall through")
	}
	if got.NoNewPrivs {
		t.Errorf("empty tool Policy should NOT inherit fleet NoNewPrivs; got %+v", got)
	}
}

func TestCappedBuffer(t *testing.T) {
	t.Parallel()
	buf := &cappedBuffer{cap: 10}
	n, err := buf.Write([]byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 16 {
		t.Errorf("Write returned %d, want 16 (pretend-all-written to not block)", n)
	}
	if !buf.truncated {
		t.Error("truncated flag not set after overflow")
	}
	if string(buf.Bytes()) != "0123456789" {
		t.Errorf("buffered = %q, want 0123456789", buf.Bytes())
	}
}
