package compute

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Vision, as a driver rather than a format enum.
//
// The seam is the same one chat, speech, image generation and jobs
// use: a named factory in a DriverSet, so a vendor is one file and one
// registry line rather than an arm added to every switch that builds a
// request or decodes a reply. It also keeps `driver` meaning the wire
// shape PER MODALITY — a vendor's chat protocol says nothing about its
// vision one.

// VisionRequest is one question about one image.
//
// The bytes travel, not a path. A driver that wanted a path would be
// reading a file the builtin has already opened and already checked
// against the allowed root — and a second reader is a second chance to
// read the wrong thing.
type VisionRequest struct {
	// Question is what to ask about the image.
	Question string
	// MIME is the sniffed content type. Vendors differ on whether they
	// want it as a media_type field or embedded in a data: URL, which
	// is a driver's business rather than the builtin's.
	MIME string
	// Data is the raw image. Encoding is the driver's decision.
	Data []byte
}

// VisionDriver asks one vision-capable endpoint about an image.
//
// Returns the description. Errors should be wrapped in DriverError (or
// Transient) so the failover chain can tell "try the next provider"
// from "this fails everywhere" — unclassified, every failure reads as
// permanent and the chain never advances.
type VisionDriver interface {
	Describe(ctx context.Context, req VisionRequest) (string, error)
}

// VisionDriverConfig is what every vision driver is built from.
// Fields a particular driver does not use are ignored, for the same
// reason ChatDriverConfig does it: a per-driver config type puts the
// vendor's shape back into the wiring layer this exists to keep clean.
type VisionDriverConfig struct {
	Endpoint   string
	Model      string
	Credential Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// VisionDriverFactory builds one configured vision driver.
type VisionDriverFactory func(VisionDriverConfig) (VisionDriver, error)

// RegisterVision adds a driver under name.
func (s *DriverSet) RegisterVision(name string, f VisionDriverFactory) {
	if s.vision == nil {
		s.vision = make(map[string]VisionDriverFactory)
	}
	s.vision[normaliseDriverName(name)] = f
}

// Vision builds the named driver.
//
// An unknown name is an error naming what IS registered. An operator
// who typed `driver = "antropic"` needs the list, not "not found".
func (s *DriverSet) Vision(name string, cfg VisionDriverConfig) (VisionDriver, error) {
	key := normaliseDriverName(name)
	if key == "" {
		key = DriverOpenAI
	}
	f, ok := s.vision[key]
	if !ok {
		return nil, fmt.Errorf("unknown vision driver %q; available: %s",
			name, strings.Join(s.VisionNames(), ", "))
	}
	return f(cfg)
}

// VisionNames lists the registered vision drivers, sorted.
func (s *DriverSet) VisionNames() []string { return sortedKeys(s.vision) }

// --- the OpenAI-shaped driver -----------------------------------------

// OpenAIVisionFactory adapts the /v1/chat/completions image path.
//
// Lives here rather than in a drivers package because it is the shape
// most endpoints speak, and a `drivers/openai` package importing
// compute for its request types while compute needs it as a default
// would be a cycle nobody gains from.
func OpenAIVisionFactory(cfg VisionDriverConfig) (VisionDriver, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("vision: endpoint required")
	}
	if cfg.Model == "" {
		return nil, errors.New("vision: model required")
	}
	// Inherited from a [[compute.providers]] entry, whose endpoint is
	// a BASE url. Used verbatim it POSTed to the base and returned an
	// empty-bodied 404, which the agent reported as a missing image
	// file.
	cfg.Endpoint = NormaliseEndpoint(cfg.Endpoint, ChatCompletionsPath)
	return &openAIVisionDriver{cfg: cfg, client: HTTPClientOr(cfg.HTTPClient)}, nil
}

// MockVisionFactory serves a fixed description without touching the
// network, so a node whose every provider is `driver = "mock"` can
// serve a full turn with no egress.
func MockVisionFactory(_ VisionDriverConfig) (VisionDriver, error) {
	return mockVisionDriver{}, nil
}

type mockVisionDriver struct{}

func (mockVisionDriver) Describe(_ context.Context, req VisionRequest) (string, error) {
	return "a mock description of a " + req.MIME + " image", nil
}

type openAIVisionDriver struct {
	cfg    VisionDriverConfig
	client *http.Client
}

func (d *openAIVisionDriver) Describe(ctx context.Context, req VisionRequest) (string, error) {
	body, err := json.Marshal(openAIVisionRequest{
		Model:     d.cfg.Model,
		MaxTokens: VisionMaxTokens,
		Messages: []openAIVisionMessage{{
			Role: "user",
			Content: []openAIVisionPart{
				{Type: "text", Text: req.Question},
				{Type: "image_url", ImageURL: &openAIImageURL{
					URL: dataURL(req.MIME, req.Data),
				}},
			},
		}},
	})
	if err != nil {
		return "", err
	}
	raw, err := DoVisionRequest(ctx, d.client, d.cfg, http.MethodPost, d.cfg.Endpoint, body)
	if err != nil {
		return "", err
	}
	return decodeOpenAIVision(raw)
}

// --- shared plumbing ---------------------------------------------------

// VisionMaxTokens bounds every vendor's reply. A description is a
// paragraph; an unbounded one is an unbounded bill.
const VisionMaxTokens = 1024

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// DoVisionRequest performs the call and classifies the failure.
//
// Shared so every driver classifies the same way. A driver that
// returned a bare error would look permanent to the failover chain,
// and the chain would stop on a 503 that the next provider would have
// served.
func DoVisionRequest(
	ctx context.Context,
	client *http.Client,
	cfg VisionDriverConfig,
	method, endpoint string,
	body []byte,
	extra ...Header,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for _, h := range extra {
		req.Header.Set(h.Name, h.Value)
	}
	if cfg.Credential != nil {
		if err := cfg.Credential.Apply(ctx, req); err != nil {
			return nil, err
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Transport failures are the textbook backup case: this
		// endpoint is unreachable, another may not be.
		return nil, Transient(fmt.Errorf("vision: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("vision: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 512)),
		}
	}
	return raw, nil
}

// Header is a protocol header a driver must set on every request.
//
// Distinct from a Credential: a credential is the operator's secret,
// a header like anthropic-version is the vendor's wire contract. A
// driver that expressed its protocol version as a credential would
// make the operator responsible for knowing it.
type Header struct {
	Name  string
	Value string
}

// HTTPClientOr returns c, or a client with a sane timeout.
//
// Exported because every driver package needs the same default, and a
// driver that fell back to http.DefaultClient would have no timeout at
// all — one unresponsive endpoint would hold a turn open indefinitely.
func HTTPClientOr(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 60 * time.Second}
}

type openAIVisionRequest struct {
	Model     string                `json:"model"`
	MaxTokens int                   `json:"max_tokens,omitempty"`
	Messages  []openAIVisionMessage `json:"messages"`
}

type openAIVisionMessage struct {
	Role    string             `json:"role"`
	Content []openAIVisionPart `json:"content"`
}

type openAIVisionPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

func decodeOpenAIVision(raw []byte) (string, error) {
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("no choices")
	}
	return decoded.Choices[0].Message.Content, nil
}
