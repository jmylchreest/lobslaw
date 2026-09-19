//go:build linux

package computer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
)

type ownerAuth struct{}

func (ownerAuth) AuthorizeProject(_ context.Context, owner, project string) error {
	if owner != "user:alice" || project != "project" {
		return ErrForbidden
	}
	return nil
}

type heldBrowser struct{ entered, release chan struct{} }

func (b *heldBrowser) Do(ctx context.Context, _ RoutineStep) (Result, error) {
	close(b.entered)
	select {
	case <-b.release:
		return Result{}, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
func (*heldBrowser) Close() error { return nil }

func TestTakeoverWaitsForInFlightBotAndThenExcludesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		s, _ := testService(t, t.TempDir())
		b := &heldBrowser{entered: make(chan struct{}), release: make(chan struct{})}
		s.open = func(context.Context, string) (browser, error) { return b, nil }
		botDone := make(chan error, 1)
		go func() { botDone <- s.ExecuteStep(t.Context(), "user:alice", "project", RoutineStep{Action: "capture"}) }()
		<-b.entered
		humanDone := make(chan error, 1)
		go func() {
			_, err := s.Action(t.Context(), "user:alice", "project", RoutineStep{Action: "takeover"})
			humanDone <- err
		}()
		synctest.Wait()
		select {
		case <-humanDone:
			t.Fatal("takeover acknowledged while bot still operating")
		default:
		}
		close(b.release)
		if err := <-botDone; err != nil {
			t.Fatal(err)
		}
		if err := <-humanDone; err != nil {
			t.Fatal(err)
		}
		if err := s.ExecuteStep(t.Context(), "user:alice", "project", RoutineStep{Action: "capture"}); !errors.Is(err, ErrTakeover) {
			t.Fatal(err)
		}
	})
}

func TestRecordingURLCredentialsBecomeManualPause(t *testing.T) {
	t.Parallel()
	for _, step := range []RoutineStep{
		{Action: "navigate", URL: "https://example.com/login?token=secret"},
		{Action: "navigate", URL: "https://example.com/#secret"},
		{Action: "fill", Selector: "input[name=username]", Value: "secret"},
	} {
		got := recordable(step)
		if !got.Sensitive || got.Value != "" || got.URL != "" || got.Selector != "" {
			t.Fatalf("unsafe recording: %+v", got)
		}
	}
}

type fakeBrowser struct{ calls int }

func (b *fakeBrowser) Do(_ context.Context, _ RoutineStep) (Result, error) {
	b.calls++
	return Result{}, nil
}
func (*fakeBrowser) Close() error { return nil }

func testService(t *testing.T, root string) (*Service, *fakeBrowser) {
	t.Helper()
	b := &fakeBrowser{}
	s := New(Config{Root: filepath.Join(root, "computer")}, ownerAuth{})
	s.open = func(context.Context, string) (browser, error) { return b, nil }
	t.Cleanup(func() { _ = s.Close() })
	return s, b
}

func TestOwnerGatePrecedesBrowserAccess(t *testing.T) {
	t.Parallel()
	s, b := testService(t, t.TempDir())
	for _, action := range []string{"start", "takeover", "release", "capture", "navigate", "record", "stop"} {
		_, err := s.Action(t.Context(), "user:bob", "project", RoutineStep{Action: action})
		if !errors.Is(err, ErrForbidden) {
			t.Errorf("%s: %v", action, err)
		}
	}
	if _, err := s.State(t.Context(), "user:bob", "project"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	if b.calls != 0 {
		t.Fatal("cross-owner browser invocation")
	}
}

func TestTakeoverFencesWorkerAcrossRestart(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, b := testService(t, root)
	if _, err := s.Action(t.Context(), "user:alice", "project", RoutineStep{Action: "takeover"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ExecuteStep(t.Context(), "user:alice", "project", RoutineStep{Action: "capture"}); !errors.Is(err, ErrTakeover) {
		t.Fatal(err)
	}
	if b.calls != 0 {
		t.Fatal("bot operated during takeover")
	}
	_ = s.Close()
	s, _ = testService(t, root)
	takeovers, err := s.Takeovers(t.Context(), "user:alice")
	if err != nil || len(takeovers) != 1 || takeovers[0].ProjectID != "project" {
		t.Fatalf("lost takeover attention across restart: %v %+v", err, takeovers)
	}
	other, err := s.Takeovers(t.Context(), "user:bob")
	if err != nil || len(other) != 0 {
		t.Fatalf("cross-owner takeover attention: %v %+v", err, other)
	}
	if err := s.ExecuteStep(t.Context(), "user:alice", "project", RoutineStep{Action: "capture"}); !errors.Is(err, ErrTakeover) {
		t.Fatal(err)
	}
	if _, err := s.Action(t.Context(), "user:alice", "project", RoutineStep{Action: "release"}); err != nil {
		t.Fatal(err)
	}
	if err := s.ExecuteStep(t.Context(), "user:alice", "project", RoutineStep{Action: "capture"}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingNeverContainsTypedValues(t *testing.T) {
	t.Parallel()
	s, _ := testService(t, t.TempDir())
	for _, step := range []RoutineStep{{Action: "takeover"}, {Action: "record"}, {Action: "fill", Selector: "#password", Value: "highly-secret"}, {Action: "navigate", URL: "https://example.com/"}} {
		if _, err := s.Action(t.Context(), "user:alice", "project", step); err != nil {
			t.Fatal(err)
		}
	}
	state, err := s.State(t.Context(), "user:alice", "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Steps) != 2 || !state.Steps[0].Sensitive || state.Steps[0].Value != "" {
		t.Fatalf("unsafe recording: %#v", state.Steps)
	}
	if err := s.ExecuteStep(t.Context(), "user:alice", "project", RoutineStep{Action: "fill", Sensitive: true}); !errors.Is(err, ErrManual) && !errors.Is(err, ErrTakeover) {
		t.Fatal(err)
	}
	for _, step := range state.Steps {
		if strings.Contains(step.Value, "secret") {
			t.Fatal("secret in draft")
		}
	}
}

func TestOnlyOneControllerMayOwnPrivateRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, _ := testService(t, root)
	if _, err := first.State(t.Context(), "user:alice", "project"); err != nil {
		t.Fatal(err)
	}
	second, _ := testService(t, root)
	if _, err := second.State(t.Context(), "user:alice", "project"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("two controllers acquired root: %v", err)
	}
	_ = first.Close()
	if _, err := second.State(t.Context(), "user:alice", "project"); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedReplayNeverOptsAutomaticRecordingIntoValues(t *testing.T) {
	t.Parallel()
	step := RoutineStep{Action: "fill", Selector: "html:nth-of-type(1) > body:nth-of-type(1) > input:nth-of-type(1)", Value: "private-value", InputMode: InputReviewedLiteral}
	recorded := recordable(step)
	if recorded.Selector != step.Selector || !recorded.Sensitive || recorded.Value != "" || recorded.InputMode != "" {
		t.Fatalf("automatic recording retained an input: %+v", recorded)
	}
	for _, action := range []string{"click", "wait", "navigate"} {
		filtered := recordable(RoutineStep{Action: action, Selector: "body:nth-of-type(1)", URL: "https://example.com/", Value: "unused-secret", InputMode: InputReviewedLiteral})
		if filtered.Value != "" || filtered.InputMode != "" {
			t.Fatalf("irrelevant fields recorded: %+v", filtered)
		}
	}
	s, b := testService(t, t.TempDir())
	for _, mode := range []string{"", "unreviewed"} {
		step.InputMode = mode
		if err := s.ExecuteStep(t.Context(), "user:alice", "project", step); !errors.Is(err, ErrManual) {
			t.Fatalf("unreviewed replay accepted: %v", err)
		}
	}
	if b.calls != 0 {
		t.Fatal("unreviewed fill reached browser")
	}
	step.InputMode = InputReviewedLiteral
	if err := s.ExecuteStep(t.Context(), "user:alice", "project", step); err != nil {
		t.Fatal(err)
	}
	if b.calls != 1 {
		t.Fatal("reviewed fill did not reach runtime's field-classification gate")
	}
}

func TestLegacyProfileMovesWithoutMovingHostControl(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "profile"), privateDirMode); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"control.json": `{"control":"human"}`, "profile/cookies": "private profile marker", "location": "https://example.com"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), privateFileMode); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := executionDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "control.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("control metadata moved into browser grant")
	}
	data, err := os.ReadFile(filepath.Join(dir, "profile", "cookies"))
	if err != nil || string(data) != "private profile marker" {
		t.Fatal("profile lost during isolation")
	}
	data, err = os.ReadFile(filepath.Join(root, "control.json"))
	if err != nil || string(data) != `{"control":"human"}` {
		t.Fatal("takeover metadata lost during isolation")
	}
}
