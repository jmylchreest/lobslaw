package embedder

import "math"

// The forward pass. Post-LayerNorm transformer encoder:
//
//	h = LN(word[id] + pos[i+posOff] + type[0])
//	per layer:
//	  h = LN(h + Wo·attention(Q,K,V) + bo)
//	  h = LN(h + Wd·gelu(Wi·h + bi) + bd)
//	pool, then L2-normalise
//
// Post-LN, not pre-LN: the normalisation comes AFTER each residual
// add. Modern decoders are usually pre-LN, so this is a natural thing
// to get backwards, and doing so produces finite output that is
// simply wrong.

// Embed returns the pooled, L2-normalised sentence vector for a
// sequence of token ids.
//
// Takes IDS, not text, which is the seam the whole package is built
// around: the numerics can be gated against reference vectors without
// a tokenizer existing, so a numerical bug and a tokenization bug can
// never be confused for one another.
//
// Ids are expected to already carry the model's special tokens.
func (m *Model) Embed(ids []int32) []float32 {
	h, rows := m.hiddenStates(ids)
	var v []float32
	if m.pool == PoolCLS {
		v = clsPool(h, m.cfg.Hidden)
	} else {
		v = meanPool(h, rows, m.cfg.Hidden)
	}
	return l2norm(v)
}

// hiddenStates runs the transformer and returns the last hidden state
// [rows, hidden] together with the row count actually produced.
//
// rows is returned rather than derived by the caller because ids are
// truncated here: len(ids) can exceed what was processed, and pooling
// over a length the model never saw reads past the data.
func (m *Model) hiddenStates(ids []int32) ([]float32, int) {
	if len(ids) > m.maxSeq {
		ids = ids[:m.maxSeq]
	}
	L, D := len(ids), m.cfg.Hidden
	if L == 0 {
		return nil, 0
	}
	c := m.cfg
	headDim := D / c.Heads

	// --- embeddings -------------------------------------------------
	h := make([]float32, L*D)
	vocab := len(m.wordEmb) / D
	typeVocab := 0
	if D > 0 {
		typeVocab = len(m.typeEmb) / D
	}
	for i, id := range ids {
		// Clamped rather than trusted. Ids arrive from a tokenizer,
		// and an out-of-range one would index past the table into
		// whatever follows it — a panic at best.
		w := m.wordEmb[clampID(id, vocab)*D:][:D]
		pos := m.posEmb[(i+m.posOff)*D:][:D]
		row := h[i*D : (i+1)*D]
		for j := range D {
			row[j] = w[j] + pos[j]
		}
		if typeVocab > 0 {
			typ := m.typeEmb[:D]
			for j := range D {
				row[j] += typ[j]
			}
		}
	}
	layerNorm(h, m.embLNW, m.embLNB, L, D, c.LNEps)

	// Scratch reused across all layers. Allocating inside the loop
	// would mean ~10 allocations per layer per call, which at 24
	// layers dominates the profile of a short sequence.
	q := make([]float32, L*D)
	k := make([]float32, L*D)
	v := make([]float32, L*D)
	ctx := make([]float32, L*D)
	out := make([]float32, L*D)
	inter := make([]float32, L*c.Intermediate)
	qh := make([]float32, L*headDim)
	kh := make([]float32, L*headDim)
	vht := make([]float32, headDim*L)
	ctxHead := make([]float32, L*headDim)
	scores := make([]float32, L*L)

	for li := range m.layers {
		l := &m.layers[li]

		matmulBT(h, l.Wq, q, L, D, D)
		matmulBT(h, l.Wk, k, L, D, D)
		matmulBT(h, l.Wv, v, L, D, D)
		addBias(q, l.Bq, L, D)
		addBias(k, l.Bk, L, D)
		addBias(v, l.Bv, L, D)

		m.attention(q, k, v, ctx, qh, kh, vht, ctxHead, scores, c.Heads, headDim, D, L)

		matmulBT(ctx, l.Wo, out, L, D, D)
		addBias(out, l.Bo, L, D)
		for i := range h {
			h[i] += out[i]
		}
		layerNorm(h, l.AttnLNW, l.AttnLNB, L, D, c.LNEps)

		matmulBT(h, l.Wi, inter, L, D, c.Intermediate)
		addBias(inter, l.Bi, L, c.Intermediate)
		gelu(inter)
		matmulBT(inter, l.Wd, out, L, c.Intermediate, D)
		addBias(out, l.Bd, L, D)
		for i := range h {
			h[i] += out[i]
		}
		layerNorm(h, l.OutLNW, l.OutLNB, L, D, c.LNEps)
	}
	return h, L
}

// attention is multi-head scaled dot-product attention, one head at a
// time into a shared scratch.
//
// The scale is 1/sqrt(headDim), NOT 1/sqrt(hidden). At 768 hidden and
// 12 heads that is 1/sqrt(64) against 1/sqrt(768) — a factor of 3.5
// on every score, which softmax turns into a distribution that is far
// too flat. The output stays finite and plausible.
//
// V is written TRANSPOSED into vht so the context matmul can read it
// as [headDim, L] rows, matching matmulBT's b-is-[n,k] contract
// without a second transpose.
func (m *Model) attention(q, k, v, ctx, qh, kh, vht, ctxHead, scores []float32, heads, headDim, dim, seq int) {
	scale := float32(1 / math.Sqrt(float64(headDim)))
	for head := range heads {
		for i := range seq {
			src := i*dim + head*headDim
			copy(qh[i*headDim:(i+1)*headDim], q[src:src+headDim])
			copy(kh[i*headDim:(i+1)*headDim], k[src:src+headDim])
			for d := range headDim {
				vht[d*seq+i] = v[src+d]
			}
		}
		matmulBT(qh, kh, scores, seq, headDim, seq)
		for i := range scores[:seq*seq] {
			scores[i] *= scale
		}
		softmaxRows(scores, seq, seq)
		matmulBT(scores, vht, ctxHead, seq, seq, headDim)
		for i := range seq {
			dst := i*dim + head*headDim
			copy(ctx[dst:dst+headDim], ctxHead[i*headDim:(i+1)*headDim])
		}
	}
}

// clampID keeps an out-of-range token id inside the vocabulary.
func clampID(id int32, vocab int) int {
	if id < 0 || int(id) >= vocab {
		return 0
	}
	return int(id)
}
