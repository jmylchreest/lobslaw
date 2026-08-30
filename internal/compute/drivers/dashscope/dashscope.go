// Package dashscope implements video generation against Alibaba's
// DashScope API (the Wan models).
//
// It is the first real JobDriver, and it was chosen over Veo and
// Bedrock because it is the least like the synchronous drivers: an
// opaque task_id, a submit that needs a header to be asynchronous at
// all, and a poll on a different path. If the job waist carries this
// it can carry the other two, whose handles are an operation resource
// name and an ARN respectively.
package dashscope

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/textutil"
)

// errTaskFailedNoReason stands in when DashScope reports a failure
// with neither a code nor a message. An empty error reads as success
// to everything downstream, which is the one thing it must not do.
const errTaskFailedNoReason = "task failed without a reason"

const (
	// DefaultSubmitEndpoint is the text-to-video synthesis path.
	DefaultSubmitEndpoint = "https://dashscope-intl.aliyuncs.com/api/v1/services/aigc/video-generation/video-synthesis"

	// DefaultTaskEndpoint is where a task is polled. The id is
	// appended; it is NOT the submit path, which is the first thing a
	// design assuming "GET the submit URL" gets wrong.
	//
	// Only reached when SubmitEndpoint is also defaulted — otherwise
	// the poll host is derived from the submit host by taskPath.
	DefaultTaskEndpoint = "https://dashscope-intl.aliyuncs.com/api/v1/tasks/"

	// taskPath is the poll path on whichever DashScope host submitted
	// the job. It is a constant across every deployment; only the host
	// moves.
	taskPath = "/api/v1/tasks/"

	// DriverName is embedded in every handle this driver mints, so it
	// must stay stable across restarts: a handle stored under one name
	// cannot be polled after a rename.
	DriverName = "dashscope"

	// defaultPollInterval follows the vendor's guidance against
	// runtimes of roughly one to five minutes.
	defaultPollInterval = 15 * time.Second
)

type Config struct {
	SubmitEndpoint string
	TaskEndpoint   string
	Model          string
	Credential     compute.Credential
	HTTPClient     *http.Client
	PollEvery      time.Duration
}

type Driver struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*Driver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("dashscope: credential required")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("dashscope: model required")
	}
	if cfg.SubmitEndpoint == "" {
		cfg.SubmitEndpoint = DefaultSubmitEndpoint
	}
	if cfg.TaskEndpoint == "" {
		cfg.TaskEndpoint = taskEndpointFor(cfg.SubmitEndpoint)
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultPollInterval
	}
	c := cfg.HTTPClient
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	return &Driver{cfg: cfg, client: c}, nil
}

func (d *Driver) PollInterval() time.Duration { return d.cfg.PollEvery }

// taskEndpointFor derives the poll URL from the submit URL.
//
// Submit and poll are always the same host — the task only exists on
// the deployment that accepted it — so pinning the poll host to a
// compiled-in default breaks every DashScope deployment that is not
// the international pay-as-you-go one. A Token Plan subscription
// (token-plan.<region>.maas.aliyuncs.com, sk-sp- keys) submits
// happily and then polls dashscope-intl, where its key is not valid:
// HTTP 401 InvalidApiKey forever, on a job that is running and billed.
// The same trap catches the Beijing and US hosts.
//
// Only the host is taken from the submit URL. The path is not
// derivable — submit is a service path and poll is /api/v1/tasks/ —
// which is exactly what JobDriverConfig means by leaving the second
// URL to the driver.
func taskEndpointFor(submit string) string {
	u, err := url.Parse(submit)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return DefaultTaskEndpoint
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: taskPath}).String()
}

type submitWire struct {
	Model string `json:"model"`
	Input struct {
		Prompt string `json:"prompt"`
	} `json:"input"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

type submitResponse struct {
	Output struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	} `json:"output"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Submit starts a job. The X-DashScope-Async header is what makes it
// asynchronous — without it the request blocks and then times out, so
// it is not optional decoration.
func (d *Driver) Submit(ctx context.Context, req compute.JobRequest) (compute.JobHandle, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("dashscope: empty prompt"))
	}
	model := req.Model
	if model == "" {
		model = d.cfg.Model
	}

	var body submitWire
	body.Model = model
	body.Input.Prompt = prompt
	if len(req.Options) > 0 {
		body.Parameters = req.Options
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("dashscope: marshal: %w", err))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.cfg.SubmitEndpoint, bytes.NewReader(raw))
	if err != nil {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("dashscope: build request: %w", err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-DashScope-Async", "enable")
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("dashscope: apply credential: %w", err))
	}

	respBody, err := d.do(ctx, httpReq, "submit")
	if err != nil {
		return compute.JobHandle{}, err
	}
	var out submitResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("dashscope: malformed submit response: %w", err))
	}
	if out.Output.TaskID == "" {
		return compute.JobHandle{}, compute.Transient(fmt.Errorf(
			"dashscope: submit returned no task_id (code=%q message=%q)", out.Code, out.Message))
	}
	return compute.JobHandle{Driver: DriverName, Raw: out.Output.TaskID}, nil
}

type taskResponse struct {
	Output struct {
		TaskStatus string `json:"task_status"`
		VideoURL   string `json:"video_url"`
		Message    string `json:"message"`
		Code       string `json:"code"`
	} `json:"output"`
	Usage struct {
		Duration   float64 `json:"duration"`
		VideoCount int     `json:"video_count"`
	} `json:"usage"`
}

func (d *Driver) Poll(ctx context.Context, h compute.JobHandle) (compute.JobState, error) {
	if h.Driver != DriverName {
		return compute.JobState{}, compute.Permanent(fmt.Errorf(
			"dashscope: handle belongs to driver %q", h.Driver))
	}
	url := d.cfg.TaskEndpoint + h.Raw
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return compute.JobState{}, compute.Permanent(fmt.Errorf("dashscope: build poll: %w", err))
	}
	if err := d.cfg.Credential.Apply(ctx, httpReq); err != nil {
		return compute.JobState{}, compute.Permanent(fmt.Errorf("dashscope: apply credential: %w", err))
	}

	respBody, err := d.do(ctx, httpReq, "poll")
	if err != nil {
		return compute.JobState{}, err
	}
	var out taskResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return compute.JobState{}, compute.Permanent(fmt.Errorf("dashscope: malformed task response: %w", err))
	}

	st := compute.JobState{Status: normaliseTaskStatus(out.Output.TaskStatus)}
	switch st.Status {
	case compute.JobSucceeded:
		if out.Output.VideoURL == "" {
			return compute.JobState{}, compute.Transient(fmt.Errorf(
				"dashscope: task succeeded with no video_url"))
		}
		st.Artifact = &compute.Artifact{
			Kind: compute.ArtifactURL,
			URL:  out.Output.VideoURL,
			MIME: "video/mp4",
			// The vendor holds generated video for 24 hours. Recording
			// it lets the resolver refuse a fetch it knows is too late
			// instead of reporting a confusing 403 from a CDN.
			ExpiresAt: time.Now().Add(24 * time.Hour),
		}
		st.Usage = compute.ModalUsage{
			Unit:     compute.UnitVideoSeconds,
			Quantity: out.Usage.Duration,
			BilledTo: compute.BillingBalance,
		}
	case compute.JobFailed:
		st.Err = strings.TrimSpace(out.Output.Code + " " + out.Output.Message)
		if st.Err == "" {
			st.Err = errTaskFailedNoReason
		}
	}
	return st, nil
}

// normaliseTaskStatus maps the vendor's enum onto the four states the
// scheduler branches on. An unrecognised status counts as running:
// the poll loop is bounded by a deadline anyway, and guessing
// "failed" would abandon a job that is still being billed.
func normaliseTaskStatus(s string) compute.JobStatus {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "SUCCEEDED":
		return compute.JobSucceeded
	case "FAILED", "CANCELED", "CANCELLED", "UNKNOWN":
		return compute.JobFailed
	case "PENDING":
		return compute.JobPending
	default:
		return compute.JobRunning
	}
}

func (d *Driver) do(ctx context.Context, req *http.Request, what string) ([]byte, error) {
	resp, err := d.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("dashscope: %s: %w", what, ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("dashscope: %s: %w", what, err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("dashscope: %s: read: %w", what, readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("dashscope: %s: HTTP %d: %s", what, resp.StatusCode, textutil.Truncate(string(raw), "…[truncated]", 512)),
		}
	}
	return raw, nil
}
