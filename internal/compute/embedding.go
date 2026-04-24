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

// EmbeddingClientConfig configures the OpenAI-compat client.
type EmbeddingClientConfig struct {
	// Endpoint accepts either the base URL
	// ("https://openrouter.ai/api/v1") or the full /embeddings URL.
	// The client normalises the suffix the same way LLMClient does
	// for /chat/completions.
	Endpoint string

	APIKey string

	// Model is the embedding model name. Examples:
	//   openai/text-embedding-3-small  (1536 dims)
	//   openai/text-embedding-3-large  (3072 dims)
	//   voyage-3                       (1024 dims)
	Model string

	// Dims tells callers how big the returned vectors are, for
	// pre-allocation. Must match the model's actual output
	// dimension — callers that guess wrong get runtime length-
	// mismatch errors downstream.
	Dims int

	Timeout time.Duration

	HTTPClient *http.Client

	Logger *slog.Logger
}

// EmbeddingClient speaks the OpenAI /embeddings wire format.
type EmbeddingClient struct {
	endpoint   string
	apiKey     string
	model      string
	dims       int
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
	return &EmbeddingClient{
		endpoint:   endpoint,
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		dims:       cfg.Dims,
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
	reqBody, _ := json.Marshal(openAIEmbeddingRequest{
		Input: text,
		Model: c.model,
	})
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
	var decoded openAIEmbeddingResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("Embed: decode: %w", err)
	}
	if len(decoded.Data) == 0 {
		return nil, errors.New("Embed: empty response data")
	}
	vec := decoded.Data[0].Embedding
	if len(vec) != c.dims {
		return nil, fmt.Errorf("Embed: model returned %d dims, expected %d (check config.dims matches the model)", len(vec), c.dims)
	}
	c.log.Debug("embed", "dims", len(vec), "duration", time.Since(start))
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
