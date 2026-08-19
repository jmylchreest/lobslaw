package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/egress"
)

// EmbeddingProvider returns a vector embedding for an input string.
// Implementations hit an OpenAI-compat /embeddings endpoint; the
// one Provider interface keeps memory_search + context-engine free
// of HTTP details.
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// EmbedBatch returns one vector per input string in the same
	// order. Providers that support batch requests (most do —
	// OpenAI-compat /embeddings takes input as either a string
	// or a string array) fold N texts into one round-trip.
	// Providers without native batch fall back to sequential
	// Embed calls, so callers can always batch without checking
	// capability.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
	Dimensions() int
	// Model is the configured model string, stamped onto every record
	// written from these vectors so a later swap is detectable.
	Model() string
}

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

	// DriverFactory builds the wire protocol.
	//
	// A FACTORY rather than a built driver, because the endpoint
	// suffix rule below runs first and the driver needs the
	// normalised endpoint. Nil takes the OpenAI shape, which is what
	// an unset format used to mean.
	DriverFactory EmbeddingDriverFactory

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
	driver     EmbeddingDriver
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
		// Embedding traffic shares the "embedding" role in the
		// egress ACL — by default the same hosts as "llm" since
		// most providers bundle embeddings with chat. Operators
		// who run a separate embedding provider get its host
		// rolled into the same allowlist by the ACL builder.
		base := egress.For("embedding").HTTPClient()
		wrapped := *base
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		wrapped.Timeout = timeout
		hc = &wrapped
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	factory := cfg.DriverFactory
	if factory == nil {
		factory = OpenAIEmbeddingFactory
	}
	driver, err := factory(EmbeddingDriverConfig{
		Endpoint:   endpoint,
		Model:      cfg.Model,
		Credential: embeddingCredential(cfg.APIKey),
		HTTPClient: hc,
		Logger:     logger,
	})
	if err != nil {
		return nil, fmt.Errorf("EmbeddingClient: %w", err)
	}
	return &EmbeddingClient{
		endpoint:   endpoint,
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		dims:       cfg.Dims,
		driver:     driver,
		httpClient: hc,
		log:        logger,
	}, nil
}

// Dimensions reports the vector length Embed returns.
func (c *EmbeddingClient) Dimensions() int { return c.dims }

// EmbedBatch dispatches one HTTP call per batch. OpenAI-compat
// providers accept `input` as a string array; MiniMax accepts
// `texts` as an array natively. Both return N vectors in order.
//
// Empty input returns an empty slice without a round-trip.
// Single-item batches delegate to Embed to keep the simple case
// on the cached single-element path.
func (c *EmbeddingClient) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) == 1 {
		vec, err := c.Embed(ctx, texts[0])
		if err != nil {
			return nil, err
		}
		return [][]float32{vec}, nil
	}
	// Filter empty strings — they fail on the single-Embed path
	// and there's no sensible vector for a zero-norm input.
	nonEmpty := make([]string, 0, len(texts))
	originalIdx := make([]int, 0, len(texts))
	for i, t := range texts {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		nonEmpty = append(nonEmpty, t)
		originalIdx = append(originalIdx, i)
	}
	if len(nonEmpty) == 0 {
		return nil, errors.New("EmbedBatch: all inputs empty after trimming")
	}

	start := time.Now()
	vectors, err := c.driver.EmbedBatch(ctx, nonEmpty)
	if err != nil {
		return nil, err
	}

	// Dim-check the first vector (all should match).
	if len(vectors) > 0 && len(vectors[0]) != c.dims {
		return nil, fmt.Errorf("EmbedBatch: model returned %d dims, expected %d", len(vectors[0]), c.dims)
	}

	// Re-project into the caller's original slot order (empty
	// inputs stay nil).
	out := make([][]float32, len(texts))
	for i, origIdx := range originalIdx {
		out[origIdx] = vectors[i]
	}

	c.log.Debug("embed.batch",
		"count", len(nonEmpty),
		"dims", c.dims, "duration", time.Since(start))
	return out, nil
}

// Embed returns the vector for text. Empty text returns an error
// rather than a zero vector because downstream similarity math
// falls apart on zero-norm inputs.
func (c *EmbeddingClient) Embed(ctx context.Context, text string) ([]float32, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, errors.New("Embed: input text is empty")
	}

	start := time.Now()
	vec, err := c.driver.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

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
	Vectors  [][]float32     `json:"vectors"`
	BaseResp minimaxBaseResp `json:"base_resp"`
}

type minimaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

// embeddingCredential presents the key as a bearer token, or nothing
// when there is none — a self-hosted embedding server usually wants no
// auth at all, and sending "Bearer " with an empty value is worse than
// sending nothing.
func embeddingCredential(apiKey string) Credential {
	if apiKey == "" {
		return nil
	}
	return NewBearerCredential(apiKey)
}

// Model returns the configured embedding model string.
func (c *EmbeddingClient) Model() string { return c.model }
