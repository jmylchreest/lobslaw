package embedder

import (
	"fmt"
	"path/filepath"
)

// Encoder is a loaded model and its tokenizer: text in, vector out.
//
// The two are kept separate underneath — Embed takes ids, LoadTokenizer
// is its own entry point — because that seam is what let the numerics
// be gated against reference vectors before any tokenizer existed, so a
// forward-pass bug and a tokenization bug could never be mistaken for
// one another. This is the join, for callers who just want an embedding.
//
// Immutable after Open and safe for concurrent use.
type Encoder struct {
	model *Model
	tok   *Tokenizer
}

// Open loads a checkpoint directory.
func Open(dir string) (*Encoder, error) {
	m, err := Load(dir)
	if err != nil {
		return nil, err
	}
	tok, err := LoadTokenizer(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("embedder: %w", err)
	}
	return &Encoder{model: m, tok: tok}, nil
}

// Close releases the checkpoint mapping.
func (e *Encoder) Close() error { return e.model.Close() }

// Dim is the embedding width.
func (e *Encoder) Dim() int { return e.model.Dim() }

// MaxSeq is the model's context limit in tokens.
func (e *Encoder) MaxSeq() int { return e.model.MaxSeq() }

// Encode returns the embedding for text.
//
// Text longer than the model's context is CHUNKED rather than
// truncated. Truncation is the silent option: a long memory would have
// its tail discarded and recall could never match anything said in the
// second half, with nothing anywhere reporting a problem.
func (e *Encoder) Encode(text string) []float32 {
	ids := e.tok.EncodeWithSpecials(text, e.model.MaxSeq())
	// Re-tokenised without the length cap to find out whether the cap
	// actually bit. EncodeWithSpecials truncates, so its output alone
	// cannot distinguish "exactly at the limit" from "cut short".
	if full := e.tok.Encode(text); len(full)+2 > e.model.MaxSeq() {
		return e.model.EmbedLong(
			e.tok.EncodeWithSpecials(text, len(full)+2),
			e.tok.BOS(), e.tok.EOS())
	}
	return e.model.Embed(ids)
}

// EncodeBatch returns one embedding per input, in order.
func (e *Encoder) EncodeBatch(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = e.Encode(t)
	}
	return out
}
