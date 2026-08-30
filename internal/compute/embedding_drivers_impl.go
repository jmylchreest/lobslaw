package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// The two embedding wire shapes.
//
// Each owns its request, its decode and its own idea of what an error
// looks like — MiniMax reports failure in a body field on an HTTP 200,
// which is exactly the sort of vendor detail that has no business in a
// shared client.

// --- OpenAI: {input, model} -> {data: [{embedding, index}]} ----------

type openAIEmbeddingDriver struct {
	cfg    EmbeddingDriverConfig
	client *http.Client
}

func (d *openAIEmbeddingDriver) Embed(ctx context.Context, text string) ([]float32, error) {
	// `input` is a bare STRING here and an array in EmbedBatch. That
	// difference is why the interface has both methods: collapsing
	// them would change the bytes every existing deployment sends.
	body, err := json.Marshal(openAIEmbeddingRequest{Input: text, Model: d.cfg.Model})
	if err != nil {
		return nil, err
	}
	raw, err := doEmbeddingRequest(ctx, d.client, d.cfg, body)
	if err != nil {
		return nil, err
	}
	var decoded openAIEmbeddingResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("embed: decode (openai): %w", err)
	}
	if len(decoded.Data) == 0 {
		return nil, errors.New("embed: empty response data (openai)")
	}
	return decoded.Data[0].Embedding, nil
}

func (d *openAIEmbeddingDriver) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(struct {
		Input []string `json:"input"`
		Model string   `json:"model"`
	}{Input: texts, Model: d.cfg.Model})
	if err != nil {
		return nil, err
	}
	raw, err := doEmbeddingRequest(ctx, d.client, d.cfg, body)
	if err != nil {
		return nil, err
	}
	var decoded openAIEmbeddingResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("embed: decode (openai): %w", err)
	}
	if len(decoded.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors, sent %d inputs", len(decoded.Data), len(texts))
	}
	// Placed BY INDEX, not by arrival order. The API does not promise
	// the array comes back sorted, and a batch reassembled in the wrong
	// order attaches every memory to the wrong text — which nothing
	// downstream can detect, because a vector is a plausible vector
	// whichever text it came from.
	vectors := make([][]float32, len(decoded.Data))
	for _, item := range decoded.Data {
		if item.Index < 0 || item.Index >= len(decoded.Data) {
			return nil, fmt.Errorf("embed: openai returned out-of-range index %d", item.Index)
		}
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}

// --- MiniMax: {texts, model, type} -> {vectors} ----------------------

type minimaxEmbeddingDriver struct {
	cfg    EmbeddingDriverConfig
	client *http.Client
}

func (d *minimaxEmbeddingDriver) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := d.embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, errors.New("embed: empty response data (minimax)")
	}
	return vectors[0], nil
}

func (d *minimaxEmbeddingDriver) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	vectors, err := d.embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors, sent %d inputs", len(vectors), len(texts))
	}
	return vectors, nil
}

// embed is the one wire call. MiniMax takes an array either way, so
// unlike OpenAI its single and batch paths are genuinely the same
// request.
func (d *minimaxEmbeddingDriver) embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(minimaxEmbeddingRequest{
		Texts: texts,
		Model: d.cfg.Model,
		Type:  "db",
	})
	if err != nil {
		return nil, err
	}
	raw, err := doEmbeddingRequest(ctx, d.client, d.cfg, body)
	if err != nil {
		return nil, err
	}
	var decoded minimaxEmbeddingResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("embed: decode (minimax): %w", err)
	}
	// MiniMax reports failure in base_resp even on an HTTP 200. Without
	// this the caller gets zero vectors and no reason.

	if decoded.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("embed: minimax status %d: %s",
			decoded.BaseResp.StatusCode, decoded.BaseResp.StatusMsg)
	}
	return decoded.Vectors, nil
}

// --- shared plumbing -------------------------------------------------

// doEmbeddingRequest performs the call and classifies the failure, so
// every driver classifies the same way.
func doEmbeddingRequest(
	ctx context.Context,
	client *http.Client,
	cfg EmbeddingDriverConfig,
	body []byte,
) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cfg.Credential != nil {
		if err := cfg.Credential.Apply(ctx, req); err != nil {
			return nil, err
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, Transient(fmt.Errorf("embed: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, TruncateBodyFor(raw, 256)),
		}
	}
	return raw, nil
}
