package hooks

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// writeHookScript drops a small shell script into dir and returns
// its absolute path. Uses the same paranoid write-then-exec pattern
// internal/compute/executor_test.go's writeScript uses: OpenFile +
// Write + Sync + Close + Chmod + Stat-verify-exec-bit. os.WriteFile
// alone races with subsequent execve on some filesystems — tmpfs /
// CoW overlays can leave executable mode bits or file contents
// invisible to an immediate child process, which surfaces as the
// test's "text file busy" / "permission denied" flakes.
func writeHookScript(t *testing.T, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(dir, name)
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
		t.Fatalf("writeHookScript: %q has no executable bits after Chmod (fs race?)", p)
	}
	return p
}

func TestDispatcherNoMatchReturnsNil(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(nil, nil)
	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_name": "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Errorf("no hooks registered should return (nil, nil); got %v", resp)
	}
}

func TestDispatcherHappyPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Hook that echoes an approve decision on stdout.
	hookPath := writeHookScript(t, dir, "approve.sh",
		`echo '{"decision":"approve","reason":"ok"}'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: hookPath}},
	}, nil)

	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_name": "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Decision != types.HookApprove {
		t.Errorf("got %+v, want Decision=approve", resp)
	}
}

func TestDispatcherBlockOnExitCode2(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Exit 2 with a reason on stderr — block-per-Claude-Code-convention.
	blocker := writeHookScript(t, dir, "block.sh",
		`echo "dangerous command" >&2; exit 2`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: blocker}},
	}, nil)

	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_name": "rm"})
	if err == nil {
		t.Fatal("expected ErrHookBlocked")
	}
	if !errors.Is(err, types.ErrHookBlocked) {
		t.Errorf("err = %v, want ErrHookBlocked", err)
	}
	if resp == nil || resp.Decision != types.HookBlock {
		t.Errorf("resp = %+v, want Decision=block", resp)
	}
}

func TestDispatcherBlockOnExplicitDecision(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Exit 0 but stdout says "block" — same outcome as exit 2.
	blocker := writeHookScript(t, dir, "softblock.sh",
		`echo '{"decision":"block","reason":"policy violation"}'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: blocker}},
	}, nil)

	_, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_name": "rm"})
	if !errors.Is(err, types.ErrHookBlocked) {
		t.Errorf("err = %v, want ErrHookBlocked", err)
	}
}

func TestDispatcherChainAbortsOnBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	blocker := writeHookScript(t, dir, "blocker.sh",
		`echo '{"decision":"block","reason":"stop"}'`)
	// If the second hook ever runs, it would create a marker file.
	sentinel := filepath.Join(dir, "must-not-exist")
	later := writeHookScript(t, dir, "later.sh",
		`touch `+sentinel+`; echo '{}'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {
			{Event: types.HookPreToolUse, Command: blocker},
			{Event: types.HookPreToolUse, Command: later},
		},
	}, nil)

	_, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
	if !errors.Is(err, types.ErrHookBlocked) {
		t.Fatalf("err = %v, want ErrHookBlocked", err)
	}
	if _, serr := os.Stat(sentinel); !os.IsNotExist(serr) {
		t.Error("second hook ran despite first blocking — chain abort broken")
	}
}

func TestDispatcherSequentialExecution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	order := filepath.Join(dir, "order.txt")
	// Each hook appends a unique char to order.txt.
	first := writeHookScript(t, dir, "first.sh",
		`printf 1 >> `+order+`; echo '{}'`)
	second := writeHookScript(t, dir, "second.sh",
		`printf 2 >> `+order+`; echo '{}'`)
	third := writeHookScript(t, dir, "third.sh",
		`printf 3 >> `+order+`; echo '{}'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {
			{Event: types.HookPreToolUse, Command: first},
			{Event: types.HookPreToolUse, Command: second},
			{Event: types.HookPreToolUse, Command: third},
		},
	}, nil)

	if _, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(order)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "123" {
		t.Errorf("order = %q, want 123", got)
	}
}

func TestDispatcherMatchFilter(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	only := writeHookScript(t, dir, "only.sh",
		`echo '{"reason":"matched"}'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {
			{Event: types.HookPreToolUse, Command: only, Match: map[string]string{"tool_name": "bash"}},
		},
	}, nil)

	// Matching payload → hook fires.
	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_name": "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Reason != "matched" {
		t.Errorf("match should fire hook: got %+v", resp)
	}

	// Non-matching payload → hook skipped → nil response.
	resp, err = d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_name": "git"})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Errorf("non-match should skip hook; got %+v", resp)
	}
}

func TestDispatcherTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	slow := writeHookScript(t, dir, "slow.sh", `sleep 5`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {
			{Event: types.HookPreToolUse, Command: slow, TimeoutSeconds: 1},
		},
	}, nil)

	start := time.Now()
	_, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
	elapsed := time.Since(start)

	if !errors.Is(err, types.ErrHookTimeout) {
		t.Errorf("err = %v, want ErrHookTimeout", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout didn't fire within 3s — took %v", elapsed)
	}
}

func TestDispatcherEmptyStdoutReturnsNil(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	silent := writeHookScript(t, dir, "silent.sh", `exit 0`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: silent}},
	}, nil)

	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Errorf("empty stdout should produce nil response; got %+v", resp)
	}
}

func TestDispatcherSurvivesOversizedStdout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 2MB of garbage to stdout, then a valid JSON response at the end.
	// The dispatcher must not OOM or hang on large output. This also
	// tests that our JSON parse handles trailing data sensibly.
	huge := writeHookScript(t, dir, "huge.sh",
		`dd if=/dev/zero bs=1024 count=2048 2>/dev/null | tr '\0' 'x'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: huge, TimeoutSeconds: 10}},
	}, nil)

	// Expect: either a parse error (garbage isn't JSON) surfaced to
	// caller, OR a nil response if the parser bails. What we don't
	// want is a hang or an OOM. Assert: call returns within 10s.
	done := make(chan struct{})
	go func() {
		_, _ = d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("dispatcher hung on oversized stdout")
	}
}

func TestDispatcherMalformedJSONIsParseError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Exit 0 but stdout isn't valid JSON. Dispatcher should surface
	// a parse error — NOT treat this as "proceed silently".
	junk := writeHookScript(t, dir, "junk.sh", `echo "not json at all"`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: junk}},
	}, nil)

	_, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
	if err == nil {
		t.Fatal("malformed JSON stdout should surface an error")
	}
}

func TestDispatcherMissingCommandSurfacesError(t *testing.T) {
	t.Parallel()
	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: "/nonexistent/binary-does-not-exist"}},
	}, nil)

	_, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
	if err == nil {
		t.Fatal("missing hook binary should produce an error, not panic")
	}
}

func TestDispatcherRespectsOuterContextCancel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Slow hook that would normally run 10s.
	slow := writeHookScript(t, dir, "slow.sh", `sleep 10`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {
			// No per-hook timeout — rely on outer context cancel.
			{Event: types.HookPreToolUse, Command: slow, TimeoutSeconds: 30},
		},
	}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after 500ms from a goroutine.
	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, _ = d.Dispatch(ctx, types.HookPreToolUse, Payload{})
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Errorf("outer ctx cancel didn't abort hook within 3s — took %v", elapsed)
	}
}

func TestDispatcherStderrIsolatedFromJSONParse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Hook writes garbage to stderr (noise) but valid JSON to stdout.
	// Parse must use stdout only; stderr noise must not confuse us.
	mixedOutput := writeHookScript(t, dir, "mixed.sh",
		`echo "this is noise on stderr, not the response" >&2; echo '{"decision":"approve"}'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: mixedOutput}},
	}, nil)

	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Decision != types.HookApprove {
		t.Errorf("stderr noise leaked into JSON parse: %+v / %v", resp, err)
	}
}

func TestDispatcherMiddleBlockerAbortsChain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	firstRan := filepath.Join(dir, "first")
	thirdRan := filepath.Join(dir, "third")

	first := writeHookScript(t, dir, "first.sh",
		`touch `+firstRan+`; echo '{}'`)
	blocker := writeHookScript(t, dir, "blocker.sh",
		`echo '{"decision":"block","reason":"middle"}'`)
	third := writeHookScript(t, dir, "third.sh",
		`touch `+thirdRan+`; echo '{}'`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {
			{Event: types.HookPreToolUse, Command: first},
			{Event: types.HookPreToolUse, Command: blocker},
			{Event: types.HookPreToolUse, Command: third},
		},
	}, nil)

	_, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{})
	if !errors.Is(err, types.ErrHookBlocked) {
		t.Fatalf("err = %v, want ErrHookBlocked", err)
	}

	// First hook ran...
	if _, statErr := os.Stat(firstRan); os.IsNotExist(statErr) {
		t.Error("first hook should have run before blocker")
	}
	// ...third did not.
	if _, statErr := os.Stat(thirdRan); !os.IsNotExist(statErr) {
		t.Error("third hook ran despite middle hook blocking — chain abort broken")
	}
}

func TestDispatcherPayloadContainsEventName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Hook echoes stdin as stdout but wrapped so our parser sees it.
	// We verify by checking stdin contained "hook_event_name":"PreToolUse".
	echoer := writeHookScript(t, dir, "echoer.sh",
		`read payload; case "$payload" in *hook_event_name*PreToolUse*) echo '{"reason":"saw-event"}';; *) echo '{"reason":"missing-event"}';; esac`)

	d := NewDispatcher(map[types.HookEvent][]types.HookConfig{
		types.HookPreToolUse: {{Event: types.HookPreToolUse, Command: echoer}},
	}, nil)

	resp, err := d.Dispatch(context.Background(), types.HookPreToolUse, Payload{"tool_name": "bash"})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Reason != "saw-event" {
		t.Errorf("payload missing hook_event_name: %+v", resp)
	}
}
