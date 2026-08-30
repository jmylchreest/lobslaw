package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
)

// Embeddings, as a driver rather than a format enum.
//
// The last of the four. EmbeddingFormat was threaded through the
// client struct and switched on at FIVE sites — a request build and a
// decode on each of the single and batch paths, plus validation — so a
// third vendor meant finding all five and adding a case to each.
//
// The client keeps what is genuinely its own: the endpoint suffix
// rule, the egress-scoped HTTP client, and the declared dimension. The
// driver owns the bytes.

// EmbeddingDriver turns text into vectors.
//
// Single and batch are separate methods because they are separate wire
// shapes, not because one is a convenience over the other: OpenAI's
// single call sends `input` as a bare string and its batch call sends
// an array. Collapsing them would change the bytes for every existing
// deployment to save one method.
type EmbeddingDriver interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)
}

// EmbeddingDriverConfig is what every embedding driver is built from.
//
// Endpoint is the NORMALISED one — the client appends "/embeddings"
// before the factory runs, so the suffix rule lives in one place
// rather than in each driver.
type EmbeddingDriverConfig struct {
	Endpoint   string
	Model      string
	Credential Credential
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// EmbeddingDriverFactory builds one configured embedding driver.
type EmbeddingDriverFactory func(EmbeddingDriverConfig) (EmbeddingDriver, error)

// RegisterEmbedding adds a driver under name.
func (s *DriverSet) RegisterEmbedding(name string, f EmbeddingDriverFactory) {
	if s.embedding == nil {
		s.embedding = make(map[string]EmbeddingDriverFactory)
	}
	s.embedding[normaliseDriverName(name)] = f
}

// EmbeddingFactory returns the named factory.
//
// Returns the FACTORY rather than a built driver, because the client
// normalises the endpoint before anything can be built with it.
func (s *DriverSet) EmbeddingFactory(name string) (EmbeddingDriverFactory, error) {
	key := normaliseDriverName(name)
	if key == "" {
		key = DriverOpenAI
	}
	f, ok := s.embedding[key]
	if !ok {
		return nil, fmt.Errorf("unknown embedding driver %q; available: %s",
			name, strings.Join(s.EmbeddingNames(), ", "))
	}
	return f, nil
}

// EmbeddingNames lists the registered embedding drivers, sorted.
func (s *DriverSet) EmbeddingNames() []string { return slices.Sorted(maps.Keys(s.embedding)) }

// OpenAIEmbeddingFactory adapts the /v1/embeddings shape:
// {input, model} → {data: [{embedding, index}]}.
func OpenAIEmbeddingFactory(cfg EmbeddingDriverConfig) (EmbeddingDriver, error) {
	if err := checkEmbeddingConfig(cfg); err != nil {
		return nil, err
	}
	return &openAIEmbeddingDriver{cfg: cfg, client: HTTPClientOr(cfg.HTTPClient)}, nil
}

// MiniMaxEmbeddingFactory adapts MiniMax's shape:
// {texts, model, type} → {vectors}.
func MiniMaxEmbeddingFactory(cfg EmbeddingDriverConfig) (EmbeddingDriver, error) {
	if err := checkEmbeddingConfig(cfg); err != nil {
		return nil, err
	}
	return &minimaxEmbeddingDriver{cfg: cfg, client: HTTPClientOr(cfg.HTTPClient)}, nil
}

// MockEmbeddingFactory returns deterministic vectors without touching
// the network.
func MockEmbeddingFactory(_ EmbeddingDriverConfig) (EmbeddingDriver, error) {
	return &mockEmbeddingDriver{}, nil
}

type mockEmbeddingDriver struct{}

// mockEmbeddingDims is small but not one: a single dimension would
// make every vector trivially similar and hide a bug in whatever
// consumes them.
const mockEmbeddingDims = 8

func (mockEmbeddingDriver) Embed(_ context.Context, text string) ([]float32, error) {
	return mockVector(text), nil
}

func (m mockEmbeddingDriver) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = mockVector(t)
	}
	return out, nil
}

// mockVector is a stable function of the text, so the same input
// embeds the same way twice and a similarity test means something.
func mockVector(text string) []float32 {
	v := make([]float32, mockEmbeddingDims)
	for i, r := range text {
		v[i%mockEmbeddingDims] += float32(r%97) / 97
	}
	return v
}

func checkEmbeddingConfig(cfg EmbeddingDriverConfig) error {
	if cfg.Endpoint == "" {
		return errors.New("embedding: endpoint required")
	}
	if cfg.Model == "" {
		return errors.New("embedding: model required")
	}
	return nil
}
