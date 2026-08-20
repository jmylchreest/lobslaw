package compute

import (
	"context"

	"fmt"

	"github.com/jmylchreest/lobslaw/internal/embedder"
)

// BuiltinEmbedder adapts the in-process encoder to EmbeddingProvider.
//
// The interface was written for HTTP clients, which is why it takes a
// context and returns errors on every call. Neither applies here —
// there is no request to cancel and no network to fail — but keeping
// the shape means memory_search, the episodic ingester and the context
// engine need no idea which kind they were given.
type BuiltinEmbedder struct {
	enc   *embedder.Encoder
	model string
	// Prefixes for asymmetrically-trained models; empty for the
	// symmetric default, where prefixing would make things worse.
	queryPrefix, passagePrefix string
}

// NewBuiltinEmbedder wraps a loaded encoder.
func NewBuiltinEmbedder(enc *embedder.Encoder, model string) *BuiltinEmbedder {
	return &BuiltinEmbedder{enc: enc, model: model}
}

// WithPrefixes sets the asymmetric query/passage prefixes.
func (b *BuiltinEmbedder) WithPrefixes(query, passage string) *BuiltinEmbedder {
	b.queryPrefix, b.passagePrefix = query, passage
	return b
}

// Embed returns the vector for text.
//
// An empty result is an ERROR rather than a zero vector. The encoder
// returns nothing for empty input or after Close, and passing a run of
// zeros back up would be worse than failing: it looks exactly like a
// valid embedding, so it would be written to the corpus, normalised to
// itself, and scored against every later query as a memory that matches
// nothing and reports no problem. Callers already fall back to lexical
// on an error.
func (b *BuiltinEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.encode(b.passagePrefix+text, text)
}

// EmbedQuery embeds a question rather than a stored document.
func (b *BuiltinEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return b.encode(b.queryPrefix+text, text)
}

// encode runs the model and reports the ORIGINAL text in any error,
// not the prefixed form — an operator reading "query: what do I..."
// would reasonably think the prefix was part of their data.
func (b *BuiltinEmbedder) encode(prefixed, original string) ([]float32, error) {
	vec := b.enc.Encode(prefixed)
	if len(vec) == 0 {
		return nil, fmt.Errorf("embedder: %q produced no tokens", truncateForError(original))
	}
	return vec, nil
}

// EmbedBatch returns one vector per input, in order.
//
// The ORDER AND LENGTH of the result are part of the contract:
// callers pair results with inputs positionally, so a dropped element
// would silently attach every later vector to the wrong record.
func (b *BuiltinEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := b.enc.Encode(b.passagePrefix + t)
		if len(vec) == 0 {
			return nil, fmt.Errorf("embedder: input %d (%q) produced no tokens", i, truncateForError(t))
		}
		out[i] = vec
	}
	return out, nil
}

// Dimensions is the embedding width.
func (b *BuiltinEmbedder) Dimensions() int { return b.enc.Dim() }

// Model is the identity stamped on every vector written from this
// embedder, so memory.CheckEmbeddingModel can refuse to start when the
// configured model disagrees with the corpus already on disk.
func (b *BuiltinEmbedder) Model() string { return b.model }

// Close releases the checkpoint mapping.
func (b *BuiltinEmbedder) Close() error {
	if b == nil || b.enc == nil {
		return nil
	}
	return b.enc.Close()
}

var _ EmbeddingProvider = (*BuiltinEmbedder)(nil)

// truncateForError keeps a failing input out of the log at full length.
//
// The text is a user's memory. An error message is not a place to
// reproduce one in full, and a 4,000-character log line helps nobody
// diagnose anything.
func truncateForError(s string) string {
	const max = 60
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
