// Package veo implements video generation against Vertex AI's Veo
// models.
//
// It is the second JobDriver, and every part of its protocol
// contradicts the first:
//
//	                DashScope (Wan)        Veo
//	handle          short opaque task_id   projects/…/operations/… (slashes)
//	poll            GET /tasks/{id}        POST :fetchPredictOperation
//	                                       with the name in the BODY
//	done signal     task_status enum       done: true
//	artifact        a URL, always          inline base64 OR a bucket URI
//
// JobHandle's opacity was asserted when it was written and only
// tested by a mock. A resource name with slashes, polled by POSTing it
// back, is what actually tests it: any design that treated the handle
// as an id to interpolate into a path breaks here.
package veo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/textutil"
)

const (
	DriverName = "veo"

	// defaultPollInterval sits inside the vendor's suggested 10-60s
	// against runtimes of roughly two to five minutes.
	defaultPollInterval = 20 * time.Second
)

type Config struct {
	// Endpoint is the model base, e.g.
	// https://us-central1-aiplatform.googleapis.com/v1/projects/P/locations/us-central1/publishers/google/models/veo-3.0-generate-001
	// The two operations are suffixes on it.
	Endpoint   string
	Model      string
	Credential compute.Credential
	HTTPClient *http.Client
	PollEvery  time.Duration

	// StorageURI, when set, asks Veo to write into an operator-owned
	// bucket instead of returning bytes. That is the artifact mode
	// nothing else produces, so it is worth being able to select.
	StorageURI string
}

type Driver struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) (*Driver, error) {
	if cfg.Credential == nil {
		return nil, fmt.Errorf("veo: credential required")
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("veo: endpoint required (the model base URL, including project and location)")
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
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

type submitWire struct {
	Instances  []submitInstance `json:"instances"`
	Parameters submitParams     `json:"parameters,omitempty"`
}

type submitInstance struct {
	Prompt string `json:"prompt"`
}

type submitParams struct {
	StorageURI     string `json:"storageUri,omitempty"`
	SampleCount    int    `json:"sampleCount,omitempty"`
	AspectRatio    string `json:"aspectRatio,omitempty"`
	DurationSecond int    `json:"durationSeconds,omitempty"`
}

type submitResponse struct {
	Name  string `json:"name"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (d *Driver) Submit(ctx context.Context, req compute.JobRequest) (compute.JobHandle, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("veo: empty prompt"))
	}

	body := submitWire{
		Instances: []submitInstance{{Prompt: prompt}},
		Parameters: submitParams{
			StorageURI:  d.cfg.StorageURI,
			SampleCount: 1,
			AspectRatio: req.Options["size"],
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("veo: marshal: %w", err))
	}

	out, err := d.post(ctx, d.cfg.Endpoint+":predictLongRunning", raw, "submit")
	if err != nil {
		return compute.JobHandle{}, err
	}
	var resp submitResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return compute.JobHandle{}, compute.Permanent(fmt.Errorf("veo: malformed submit response: %w", err))
	}
	if resp.Name == "" {
		msg := ""
		if resp.Error != nil {
			msg = resp.Error.Message
		}
		return compute.JobHandle{}, compute.Transient(fmt.Errorf(
			"veo: submit returned no operation name (%s)", msg))
	}
	// The whole resource name is the handle. Nothing above this line
	// parses it, which is what lets the same JobHandle carry a task id
	// and an ARN too.
	return compute.JobHandle{Driver: DriverName, Raw: resp.Name}, nil
}

type fetchWire struct {
	OperationName string `json:"operationName"`
}

type fetchResponse struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	} `json:"error"`
	Response *struct {
		Videos []struct {
			BytesBase64 string `json:"bytesBase64Encoded"`
			GCSURI      string `json:"gcsUri"`
			MIMEType    string `json:"mimeType"`
		} `json:"videos"`
	} `json:"response"`
}

// Poll POSTs the operation name back. Not a GET on the handle: the
// handle is a resource name, not a URL, and there is no path to fetch.
func (d *Driver) Poll(ctx context.Context, h compute.JobHandle) (compute.JobState, error) {
	if h.Driver != DriverName {
		return compute.JobState{}, compute.Permanent(fmt.Errorf(
			"veo: handle belongs to driver %q", h.Driver))
	}
	raw, err := json.Marshal(fetchWire{OperationName: h.Raw})
	if err != nil {
		return compute.JobState{}, compute.Permanent(fmt.Errorf("veo: marshal poll: %w", err))
	}
	out, err := d.post(ctx, d.cfg.Endpoint+":fetchPredictOperation", raw, "poll")
	if err != nil {
		return compute.JobState{}, err
	}

	var resp fetchResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return compute.JobState{}, compute.Permanent(fmt.Errorf("veo: malformed poll response: %w", err))
	}
	if !resp.Done {
		return compute.JobState{Status: compute.JobRunning}, nil
	}
	if resp.Error != nil && resp.Error.Message != "" {
		return compute.JobState{
			Status: compute.JobFailed,
			Err:    fmt.Sprintf("%d %s", resp.Error.Code, resp.Error.Message),
		}, nil
	}
	if resp.Response == nil || len(resp.Response.Videos) == 0 {
		// done with neither error nor video is the vendor contradicting
		// itself; another poll may resolve it and the loop is bounded.
		return compute.JobState{}, compute.Transient(fmt.Errorf(
			"veo: operation reported done with no video and no error"))
	}

	v := resp.Response.Videos[0]
	mime := v.MIMEType
	if mime == "" {
		mime = "video/mp4"
	}
	st := compute.JobState{
		Status: compute.JobSucceeded,
		Usage:  compute.ModalUsage{Unit: compute.UnitVideoSeconds, BilledTo: compute.BillingBalance},
	}
	switch {
	case v.GCSURI != "":
		// The provider wrote into an operator-owned bucket. This is the
		// third delivery mode, and until now only the mock produced it.
		mount, path := splitGCSURI(v.GCSURI)
		st.Artifact = &compute.Artifact{
			Kind: compute.ArtifactMount, Mount: mount, Path: path, MIME: mime,
		}
	case v.BytesBase64 != "":
		decoded, err := base64.StdEncoding.DecodeString(v.BytesBase64)
		if err != nil {
			return compute.JobState{}, compute.Permanent(fmt.Errorf("veo: decode video: %w", err))
		}
		st.Artifact = &compute.Artifact{
			Kind: compute.ArtifactInline, Bytes: decoded, MIME: mime,
		}
	default:
		return compute.JobState{}, compute.Transient(fmt.Errorf(
			"veo: video entry carried neither bytes nor a bucket URI"))
	}
	return st, nil
}

// splitGCSURI turns gs://bucket/path/to.mp4 into a mount label and a
// path. The bucket name is used as the mount label because that is
// what an operator declares a bucket mount as; a mismatch surfaces at
// resolve time as "unknown mount", which names the problem exactly.
func splitGCSURI(uri string) (mount, path string) {
	trimmed := strings.TrimPrefix(uri, "gs://")
	if i := strings.Index(trimmed, "/"); i >= 0 {
		return trimmed[:i], trimmed[i+1:]
	}
	return trimmed, ""
}

func (d *Driver) post(ctx context.Context, url string, body []byte, what string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, compute.Permanent(fmt.Errorf("veo: %s: build request: %w", what, err))
	}
	req.Header.Set("Content-Type", "application/json")
	if err := d.cfg.Credential.Apply(ctx, req); err != nil {
		return nil, compute.Permanent(fmt.Errorf("veo: %s: apply credential: %w", what, err))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, compute.Permanent(fmt.Errorf("veo: %s: %w", what, ctx.Err()))
		}
		return nil, compute.Transient(fmt.Errorf("veo: %s: %w", what, err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, compute.Transient(fmt.Errorf("veo: %s: read: %w", what, readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &compute.DriverError{
			Class: compute.ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("veo: %s: HTTP %d: %s", what, resp.StatusCode, textutil.Truncate(string(raw), "…[truncated]", 512)),
		}
	}
	return raw, nil
}
