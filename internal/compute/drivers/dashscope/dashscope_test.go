package dashscope

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

type fakeAPI struct {
	srv        *httptest.Server
	submitHdr  http.Header
	submitBody []byte
	pollPath   string
}

// newFakeAPI serves submit on one path and tasks on another, because
// conflating them is the mistake this protocol invites.
func newFakeAPI(t *testing.T, submitJSON, taskJSON string, status int) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
		f.submitHdr = r.Header.Clone()
		f.submitBody, _ = readBody(r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(submitJSON))
	})
	mux.HandleFunc("/tasks/", func(w http.ResponseWriter, r *http.Request) {
		f.pollPath = r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(taskJSON))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func readBody(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	buf := make([]byte, r.ContentLength)
	_, err := r.Body.Read(buf)
	if err != nil && err.Error() != "EOF" {
		return buf, err
	}
	return buf, nil
}

func newDriver(t *testing.T, f *fakeAPI) *Driver {
	t.Helper()
	d, err := New(Config{
		SubmitEndpoint: f.srv.URL + "/submit",
		TaskEndpoint:   f.srv.URL + "/tasks/",
		Model:          "wan2.2-t2v-plus",
		Credential:     compute.NewBearerCredential("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The async header is what makes submit asynchronous. Without it the
// request blocks and times out, so its absence is a real failure that
// would only show against the live API.
func TestSubmitSendsTheAsyncHeader(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, `{"output":{"task_id":"t-1","task_status":"PENDING"}}`, `{}`, http.StatusOK)
	d := newDriver(t, f)

	h, err := d.Submit(context.Background(), compute.JobRequest{Prompt: "a cube rotating"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.submitHdr.Get("X-DashScope-Async"); got != "enable" {
		t.Errorf("X-DashScope-Async = %q, want enable — without it the call blocks", got)
	}
	if h.Driver != DriverName || h.Raw != "t-1" {
		t.Errorf("handle = %+v, want the opaque task id under this driver", h)
	}

	var sent submitWire
	if err := json.Unmarshal(f.submitBody, &sent); err != nil {
		t.Fatalf("submit body not JSON: %v (%s)", err, f.submitBody)
	}
	if sent.Input.Prompt != "a cube rotating" {
		t.Errorf("prompt nested wrongly: %+v", sent)
	}
}

// Polling hits /tasks/{id}, not the submit path. A design that GETs
// the submit URL fits none of the three vendors surveyed.
func TestPollUsesTheTaskPath(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t,
		`{"output":{"task_id":"t-9"}}`,
		`{"output":{"task_status":"RUNNING"}}`, http.StatusOK)
	d := newDriver(t, f)

	h, _ := d.Submit(context.Background(), compute.JobRequest{Prompt: "x"})
	st, err := d.Poll(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	if f.pollPath != "/tasks/t-9" {
		t.Errorf("polled %q, want /tasks/t-9", f.pollPath)
	}
	if st.Status != compute.JobRunning {
		t.Errorf("status = %q, want running", st.Status)
	}
}

func TestPollMapsTerminalStates(t *testing.T) {
	t.Parallel()

	t.Run("succeeded carries an expiring artifact and usage", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, `{"output":{"task_id":"t"}}`,
			`{"output":{"task_status":"SUCCEEDED","video_url":"https://x.invalid/v.mp4"},
			  "usage":{"duration":5,"video_count":1}}`, http.StatusOK)
		d := newDriver(t, f)
		h, _ := d.Submit(context.Background(), compute.JobRequest{Prompt: "x"})
		st, err := d.Poll(context.Background(), h)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != compute.JobSucceeded || st.Artifact == nil {
			t.Fatalf("state = %+v, want succeeded with an artifact", st)
		}
		if st.Artifact.Kind != compute.ArtifactURL {
			t.Errorf("kind = %q, want url", st.Artifact.Kind)
		}
		if st.Artifact.ExpiresAt.IsZero() {
			t.Error("no expiry recorded; the resolver cannot tell a late fetch from a broken one")
		}
		if st.Usage.Unit != compute.UnitVideoSeconds || st.Usage.Quantity != 5 {
			t.Errorf("usage = %+v, want 5 video-seconds", st.Usage)
		}
	})

	t.Run("failed carries the reason", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, `{"output":{"task_id":"t"}}`,
			`{"output":{"task_status":"FAILED","code":"InvalidPrompt","message":"rejected"}}`,
			http.StatusOK)
		d := newDriver(t, f)
		h, _ := d.Submit(context.Background(), compute.JobRequest{Prompt: "x"})
		st, _ := d.Poll(context.Background(), h)
		if st.Status != compute.JobFailed {
			t.Fatalf("status = %q, want failed", st.Status)
		}
		if !strings.Contains(st.Err, "InvalidPrompt") {
			t.Errorf("Err = %q, want the provider's code", st.Err)
		}
	})

	t.Run("succeeded with no url is transient", func(t *testing.T) {
		t.Parallel()
		f := newFakeAPI(t, `{"output":{"task_id":"t"}}`,
			`{"output":{"task_status":"SUCCEEDED"}}`, http.StatusOK)
		d := newDriver(t, f)
		h, _ := d.Submit(context.Background(), compute.JobRequest{Prompt: "x"})
		if _, err := d.Poll(context.Background(), h); err == nil {
			t.Fatal("a success with no video_url was accepted")
		} else if compute.ClassifyFailure(err) != compute.FailureTransient {
			t.Errorf("classified %s, want transient", compute.ClassifyFailure(err))
		}
	})
}

// An unknown status means keep waiting. Guessing "failed" would
// abandon a job that is still running and still being billed; the
// poll loop's deadline is what stops it, not a guess here.
func TestUnknownStatusKeepsWaiting(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"QUEUED", "SOMETHING_NEW", ""} {
		if got := normaliseTaskStatus(s); got.Terminal() {
			t.Errorf("status %q mapped to %q; an unrecognised state must not be terminal", s, got)
		}
	}
	for in, want := range map[string]compute.JobStatus{
		"SUCCEEDED": compute.JobSucceeded,
		"FAILED":    compute.JobFailed,
		"CANCELED":  compute.JobFailed,
		"PENDING":   compute.JobPending,
	} {
		if got := normaliseTaskStatus(in); got != want {
			t.Errorf("normaliseTaskStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// A handle from another driver is a wiring bug and must fail loudly
// rather than being polled as though it were a task id.
func TestPollRejectsForeignHandle(t *testing.T) {
	t.Parallel()
	f := newFakeAPI(t, `{}`, `{}`, http.StatusOK)
	d := newDriver(t, f)
	if _, err := d.Poll(context.Background(), compute.JobHandle{Driver: "veo", Raw: "x"}); err == nil {
		t.Fatal("polled a foreign handle")
	}
}

func TestSubmitClassifiesFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		want   compute.FailureClass
	}{
		{503, compute.FailureTransient},
		{429, compute.FailureTransient},
		{400, compute.FailurePermanent},
	} {
		f := newFakeAPI(t, `{"code":"x","message":"y"}`, `{}`, tc.status)
		d := newDriver(t, f)
		_, err := d.Submit(context.Background(), compute.JobRequest{Prompt: "x"})
		if err == nil {
			t.Fatalf("HTTP %d produced no error", tc.status)
		}
		if got := compute.ClassifyFailure(err); got != tc.want {
			t.Errorf("HTTP %d classified %s, want %s", tc.status, got, tc.want)
		}
	}
}

// A job can only be polled on the host that accepted it. Pinning the
// poll host to the compiled-in international default meant a Token
// Plan deployment submitted successfully and then polled a host where
// its sk-sp- key is not valid — HTTP 401 forever, against work that
// was running and billed. Same trap for the Beijing and US hosts.
func TestTaskEndpointFollowsTheSubmitHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		submit string
		want   string
	}{
		{
			name:   "token plan",
			submit: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/api/v1/services/aigc/video-generation/video-synthesis",
			want:   "https://token-plan.ap-southeast-1.maas.aliyuncs.com/api/v1/tasks/",
		},
		{
			name:   "beijing",
			submit: "https://dashscope.aliyuncs.com/api/v1/services/aigc/video-generation/video-synthesis",
			want:   "https://dashscope.aliyuncs.com/api/v1/tasks/",
		},
		{
			name:   "unset submit falls back to the default",
			submit: "",
			want:   DefaultTaskEndpoint,
		},
		{
			name:   "unparseable submit falls back to the default",
			submit: "://not a url",
			want:   DefaultTaskEndpoint,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				SubmitEndpoint: tc.submit,
				Model:          "happyhorse-1.1-t2v",
				Credential:     compute.NewBearerCredential("k"),
			}
			d, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if got := d.cfg.TaskEndpoint; got != tc.want {
				t.Errorf("TaskEndpoint = %q, want %q", got, tc.want)
			}
		})
	}
}

// An explicit TaskEndpoint is still honoured: deriving is the default,
// not an override of what the operator asked for.
func TestExplicitTaskEndpointWins(t *testing.T) {
	d, err := New(Config{
		SubmitEndpoint: "https://token-plan.ap-southeast-1.maas.aliyuncs.com/api/v1/services/aigc/video-generation/video-synthesis",
		TaskEndpoint:   "https://elsewhere.example/api/v1/tasks/",
		Model:          "happyhorse-1.1-t2v",
		Credential:     compute.NewBearerCredential("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.cfg.TaskEndpoint, "https://elsewhere.example/api/v1/tasks/"; got != want {
		t.Errorf("TaskEndpoint = %q, want %q", got, want)
	}
}
