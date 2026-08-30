package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

type recordingStarter struct {
	calls  int
	handle compute.JobHandle
	label  string
	prompt string
	err    error
}

func (r *recordingStarter) start(_ context.Context, h compute.JobHandle, providerLabel, prompt string) (string, error) {
	r.calls++
	r.handle, r.label, r.prompt = h, providerLabel, prompt
	if r.err != nil {
		return "", r.err
	}
	return "gen-1", nil
}

func videoBuiltin(t *testing.T, d compute.JobDriver, s *recordingStarter) compute.BuiltinFunc {
	t.Helper()
	b := NewBuiltins()
	if err := RegisterVideoBuiltin(b, VideoConfig{Driver: d, Start: s.start}); err != nil {
		t.Fatal(err)
	}
	h, ok := b.Get("generate_video")
	if !ok {
		t.Fatal("generate_video not registered")
	}
	return h
}

// The tool cannot return the video, because it does not exist yet. It
// submits and hands the handle to a commitment; the scheduler finishes
// the job. Getting this wrong means the model reports a result that
// will not arrive for minutes.
func TestVideoSubmitsAndRecordsACommitment(t *testing.T) {
	t.Parallel()
	s := &recordingStarter{}
	h := videoBuiltin(t, &compute.MockJobDriver{PollsBeforeDone: 3}, s)

	out, code, err := h(context.Background(), map[string]string{
		"prompt": "a cube rotating", "size": "1280*720",
	})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if s.calls != 1 {
		t.Fatalf("starter called %d times, want 1", s.calls)
	}
	if !s.handle.Valid() {
		t.Errorf("recorded an unusable handle: %+v", s.handle)
	}
	if s.prompt != "a cube rotating" {
		t.Errorf("prompt = %q", s.prompt)
	}

	var got struct {
		Status       string `json:"status"`
		CommitmentID string `json:"commitment_id"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("result not JSON: %v (%s)", err, out)
	}
	if got.Status != "started" || got.CommitmentID != "gen-1" {
		t.Errorf("result = %+v, want a started acknowledgement carrying the commitment id", got)
	}
	// No artifact is announced: there is nothing to attach yet, and
	// announcing one here would have the channel send an empty file.
	if strings.Contains(string(out), "mount") {
		t.Errorf("result mentions a mount; nothing has been produced yet: %s", out)
	}
}

// The dangerous case. The provider has accepted the job and is billing
// for it, so a commitment that fails to record means work nothing will
// ever collect. That must surface, not be swallowed into a cheerful
// "started".
func TestVideoReportsWhenTheJobCannotBeRecorded(t *testing.T) {
	t.Parallel()
	s := &recordingStarter{err: errors.New("raft apply failed")}
	h := videoBuiltin(t, &compute.MockJobDriver{}, s)

	out, code, err := h(context.Background(), map[string]string{"prompt": "x"})
	if err == nil {
		t.Fatal("a job that could not be recorded reported success")
	}
	if code == 0 {
		t.Errorf("exit code 0 with an error; the turn would treat this as fine")
	}
	if !strings.Contains(err.Error(), "submitted") {
		t.Errorf("error should say the job WAS submitted, so an operator expects the charge: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("returned a payload alongside the failure: %s", out)
	}
}

// A submit that fails never reaches the commitment: there is no job to
// record, and writing one would create a commitment polling a handle
// that does not exist.
func TestVideoDoesNotRecordAFailedSubmit(t *testing.T) {
	t.Parallel()
	s := &recordingStarter{}
	h := videoBuiltin(t, &compute.MockJobDriver{}, s)

	// An empty prompt is rejected before submit; a driver error is
	// rejected at submit. Both must leave the commitment untouched.
	if _, _, err := h(context.Background(), map[string]string{"prompt": "  "}); err == nil {
		t.Fatal("an empty prompt was submitted")
	}
	if s.calls != 0 {
		t.Errorf("recorded a commitment for a job that was never submitted (%d calls)", s.calls)
	}
}

func TestVideoRequiresDriverAndStarter(t *testing.T) {
	t.Parallel()
	if err := RegisterVideoBuiltin(NewBuiltins(), VideoConfig{Driver: &compute.MockJobDriver{}}); err == nil {
		t.Error("registered generate_video with no way to record the job")
	}
	if err := RegisterVideoBuiltin(NewBuiltins(), VideoConfig{
		Start: (&recordingStarter{}).start,
	}); err == nil {
		t.Error("registered generate_video with no driver")
	}
}

// The provider label travels onto the commitment because the COST is
// not known until the job finishes, by which time the turn is long
// over — the only way to price seconds of video is to be able to find
// the provider's rate card again.
func TestTheProviderLabelReachesTheCommitment(t *testing.T) {
	t.Parallel()
	s := &recordingStarter{}
	b := NewBuiltins()
	d, _ := compute.MockJobFactory(compute.JobDriverConfig{})
	if err := RegisterVideoBuiltin(b, VideoConfig{
		Driver: d, Start: s.start, Label: "video-vendor",
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("generate_video")
	if _, code, err := fn(context.Background(), map[string]string{
		"prompt": "a cat on a skateboard",
	}); err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if s.label != "video-vendor" {
		t.Errorf("label = %q; the job cannot be priced when it completes", s.label)
	}
}

// countingJobDriver records whether anything polled it.
type countingJobDriver struct {
	compute.MockJobDriver
	polls int
}

func (d *countingJobDriver) Poll(ctx context.Context, h compute.JobHandle) (compute.JobState, error) {
	d.polls++
	return d.MockJobDriver.Poll(ctx, h)
}

// The turn must return IMMEDIATELY. A generate_video that waited for
// the job would hold the session lease and trip the responsiveness
// timeout long before a video exists — the whole reason this modality
// is a commitment rather than a tool call that blocks.
func TestSubmittingAVideoNeverPolls(t *testing.T) {
	t.Parallel()
	d := &countingJobDriver{MockJobDriver: compute.MockJobDriver{PollsBeforeDone: 1}}
	s := &recordingStarter{}
	b := NewBuiltins()
	if err := RegisterVideoBuiltin(b, VideoConfig{Driver: d, Start: s.start, Label: "v"}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("generate_video")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, code, err := fn(context.Background(), map[string]string{
			"prompt": "a cat on a skateboard",
		}); err != nil || code != 0 {
			t.Errorf("code=%d err=%v", code, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("generate_video did not return; it is waiting for the job")
	}

	if d.polls != 0 {
		t.Errorf("the submit path polled %d times; the turn was waiting for the job", d.polls)
	}
	if s.calls != 1 {
		t.Errorf("the job was submitted but recorded %d times; nothing would collect it", s.calls)
	}
}
