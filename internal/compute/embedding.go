package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// EmbeddingProvider returns a vector embedding for an input string.
// Implementations hit an OpenAI-compat /embeddings endpoint; the
// one Provider interface keeps memory_search + context-engine free
// of HTTP details.
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Dimensions() int
}

// EmbeddingFormat picks the request/response wire protocol the
// client speaks. Set this per-provider — the shape isn't
// auto-detectable without wasting a probe call.
type EmbeddingFormat string

const (
	// EmbeddingFormatOpenAI — {input, model} → {data: [{embedding}]}.
	// OpenAI, OpenRouter, z.ai, most hosted embedding providers.
	EmbeddingFormatOpenAI EmbeddingFormat = "openai"

	// EmbeddingFormatMiniMax — {texts, model, type} → {vectors}.
	// MiniMax's native shape via api.minimax.io/v1/embeddings.
	// Also carries a base_resp.status_code envelope for errors.
	EmbeddingFormatMiniMax EmbeddingFormat = "minimax"
)

// EmbeddingClientConfig configures the client.
type EmbeddingClientConfig struct {
	// Endpoint accepts either the base URL
	// ("https://openrouter.ai/api/v1") or the full /embeddings URL.
	// The client normalises the suffix the same way LLMClient does
	// for /chat/completions.
	Endpoint string

	APIKey string

	// Model is the embedding model name. Examples:
	//   openai/text-embedding-3-small  (OpenAI, 1536 dims)
	//   embo-01                         (MiniMax, 1536 dims)
	//   voyage-3                        (Voyage, 1024 dims)
	Model string

	// Dims tells callers how big the returned vectors are, for
	// pre-allocation. Must match the model's actual output
	// dimension — callers that guess wrong get runtime length-
	// mismatch errors downstream.
	Dims int

	// Format picks the wire protocol. Empty → openai.
	Format EmbeddingFormat

	Timeout time.Duration

	HTTPClient *http.Client

	Logger *slog.Logger
}

// EmbeddingClient dispatches /embeddings calls. Format-aware —
// same client supports OpenAI-style and MiniMax-style providers
// with identical Embed() semantics.
type EmbeddingClient struct {
	endpoint   string
	apiKey     string
	model      string
	dims       int
	format     EmbeddingFormat
	httpClient *http.Client
	log        *slog.Logger
}

// NewEmbeddingClient constructs a client, normalising the endpoint
// suffix and applying a 30s default timeout.
func NewEmbeddingClient(cfg EmbeddingClientConfig) (*EmbeddingClient, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("EmbeddingClient: endpoint is required")
	}
	if cfg.Model == "" {
		return nil, errors.New("EmbeddingClient: model is required")
	}
	if cfg.Dims <= 0 {
		return nil, errors.New("EmbeddingClient: dims must be > 0 (match the model output dimension)")
	}
	endpoint := strings.TrimRight(cfg.Endpoint, "/")
	if !strings.HasSuffix(endpoint, "/embeddings") {
		endpoint += "/embeddings"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	format := cfg.Format
	if format == "" {
		format = EmbeddingFormatOpenAI
	}
	if format != EmbeddingFormatOpenAI && format != EmbeddingFormatMiniMax {
		return nil, fmt.Errorf("EmbeddingClient: unknown format %q (want %q or %q)",
			format, EmbeddingFormatOpenAI, EmbeddingFormatMiniMax)
	}
	return &EmbeddingClient{
		endpoint:   endpoint,
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		dims:       cfg.Dims,
		format:     format,
		httpClient: hc,
		log:        logger,
	}, nil
}

// Dimensions reports the vector length Embed returns.
func (c *EmbeddingClient) Dimensions() int { return c.dims }

// Embed returns the vector for text. Empty text returns an error
// rather than a zero vector because downstream similarity math
// falls apart on zero-norm inputs.
func (c *EmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("Embed: input text is empty")
	}

	var (
		reqBody []byte
		vec     []float32
	)
	switch c.format {
	case EmbeddingFormatOpenAI:
		reqBody, _ = json.Marshal(openAIEmbeddingRequest{
			Input: text,
			Model: c.model,
		})
	case EmbeddingFormatMiniMax:
		reqBody, _ = json.Marshal(minimaxEmbeddingRequest{
			Texts: []string{text},
			Model: c.model,
			Type:  "db",
		})
	default:
		return nil, fmt.Errorf("Embed: unknown format %q", c.format)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	start := time.Now()
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("Embed: http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Embed: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 256))
	}

	switch c.format {
	case EmbeddingFormatOpenAI:
		var decoded openAIEmbeddingResponse
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("Embed: decode (openai): %w", err)
		}
		if len(decoded.Data) == 0 {
			return nil, errors.New("Embed: empty response data (openai)")
		}
		vec = decoded.Data[0].Embedding
	case EmbeddingFormatMiniMax:
		var decoded minimaxEmbeddingResponse
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("Embed: decode (minimax): %w", err)
		}
		// MiniMax returns status in base_resp even on HTTP 200.
		// status_code 0 = success; anything else surfaces as the
		// embed failure so callers see the real cause.
		if decoded.BaseResp.StatusCode != 0 {
			return nil, fmt.Errorf("Embed: minimax status %d: %s",
				decoded.BaseResp.StatusCode, decoded.BaseResp.StatusMsg)
		}
		if len(decoded.Vectors) == 0 {
			return nil, errors.New("Embed: empty vectors (minimax)")
		}
		vec = decoded.Vectors[0]
	}

	if len(vec) != c.dims {
		return nil, fmt.Errorf("Embed: model returned %d dims, expected %d (check config.dims matches the model)", len(vec), c.dims)
	}
	c.log.Debug("embed", "format", c.format, "dims", len(vec), "duration", time.Since(start))
	return vec, nil
}

type openAIEmbeddingRequest struct {
	Input string `json:"input"`
	Model string `json:"model"`
}

type openAIEmbeddingResponse struct {
	Data  []openAIEmbeddingDatum `json:"data"`
	Usage openAIUsage            `json:"usage"`
}

type openAIEmbeddingDatum struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

// MiniMax native embedding request/response shapes.
// Field naming follows their docs exactly — don't rename.

type minimaxEmbeddingRequest struct {
	Texts []string `json:"texts"`
	Model string   `json:"model"`
	// Type is "db" (for stored content) or "query" (for search
	// queries). MiniMax uses different projections depending on
	// the use — "db" on ingest, "query" on lookup. We always
	// use "db" here because this client runs for both ingest
	// (via the EpisodicIngester) and query (via memory_search),
	// and mixing the two silently would halve recall quality.
	// When we wire a search-time variant, add a separate method
	// that requests type="query".
	Type string `json:"type"`
}

type minimaxEmbeddingResponse struct {
	Vectors  [][]float32      `json:"vectors"`
	BaseResp minimaxBaseResp  `json:"base_resp"`
}

type minimaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}
