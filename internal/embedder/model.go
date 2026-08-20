package embedder

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Pooling is how token states become one sentence vector.
type Pooling string

const (
	PoolCLS  Pooling = "cls"
	PoolMean Pooling = "mean"
)

// config is the subset of HuggingFace config.json this encoder needs.
type config struct {
	ModelType     string  `json:"model_type"`
	Hidden        int     `json:"hidden_size"`
	Layers        int     `json:"num_hidden_layers"`
	Heads         int     `json:"num_attention_heads"`
	Intermediate  int     `json:"intermediate_size"`
	MaxPos        int     `json:"max_position_embeddings"`
	LNEps         float32 `json:"layer_norm_eps"`
	PadTokenID    int     `json:"pad_token_id"`
	TypeVocabSize int     `json:"type_vocab_size"`
	HiddenAct     string  `json:"hidden_act"`
}

type layer struct {
	Wq, Bq, Wk, Bk, Wv, Bv []float32
	Wo, Bo                 []float32
	AttnLNW, AttnLNB       []float32
	Wi, Bi, Wd, Bd         []float32
	OutLNW, OutLNB         []float32
}

// Model is a loaded BERT / RoBERTa / XLM-RoBERTa encoder.
//
// One implementation for all three because they are the SAME
// architecture: post-LayerNorm transformer with learned absolute
// position embeddings. bge-small (English, 12x384),
// multilingual-e5-base (12x768) and bge-m3 (multilingual, 24x1024,
// 8k context) differ only in size, vocabulary and the position
// offset below — so which one a node runs is configuration, not code.
type Model struct {
	cfg     config
	pool    Pooling
	posOff  int
	maxSeq  int
	wordEmb []float32
	posEmb  []float32
	typeEmb []float32
	embLNW  []float32
	embLNB  []float32
	layers  []layer
}

// positionOffset returns the index of the first usable position row.
//
// RoBERTa and XLM-RoBERTa reserve position ids up to and including
// pad_token_id, so token i reads position row i + pad_token_id + 1 —
// row 2 onwards, in practice. Plain BERT starts at 0.
//
// This is the single most valuable line in the package to get right
// and the easiest to omit. Reading from row 0 loads real, learned
// embeddings that are simply the wrong ones: the model runs, produces
// finite vectors, and is quietly degraded. It is also why the
// position table has 514 rows for a 512-token model — the extra two
// are the tell.
func positionOffset(modelType string, padTokenID int) int {
	switch modelType {
	case "roberta", "xlm-roberta":
		if padTokenID < 0 {
			return 0
		}
		return padTokenID + 1
	default:
		return 0
	}
}

// poolingConfig reads the sentence-transformers declaration.
//
// Which pooling to use is a property OF THE CHECKPOINT, not of the
// architecture: bge wants CLS, e5 wants mean, and both are BERT. A
// default that happens to match one of them silently degrades the
// other, so the declaration is read and only its absence falls back.
func poolingConfig(dir string, fallback Pooling) Pooling {
	raw, err := os.ReadFile(filepath.Join(dir, "1_Pooling", "config.json"))
	if err != nil {
		return fallback
	}
	var pc struct {
		CLS  bool `json:"pooling_mode_cls_token"`
		Mean bool `json:"pooling_mode_mean_tokens"`
	}
	if err := json.Unmarshal(raw, &pc); err != nil {
		return fallback
	}
	switch {
	case pc.CLS:
		return PoolCLS
	case pc.Mean:
		return PoolMean
	default:
		return fallback
	}
}

// Load reads a HuggingFace snapshot directory.
func Load(dir string) (*Model, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("embedder: read config.json: %w", err)
	}
	var cfg config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("embedder: parse config.json: %w", err)
	}
	if cfg.Hidden == 0 || cfg.Layers == 0 || cfg.Heads == 0 {
		return nil, fmt.Errorf("embedder: config.json is missing hidden_size/num_hidden_layers/num_attention_heads")
	}
	if cfg.Hidden%cfg.Heads != 0 {
		return nil, fmt.Errorf("embedder: hidden_size %d is not divisible by num_attention_heads %d", cfg.Hidden, cfg.Heads)
	}
	// Refused rather than approximated: see gelu in ops.go.
	if cfg.HiddenAct != "" && cfg.HiddenAct != "gelu" {
		return nil, fmt.Errorf("embedder: hidden_act=%q unsupported (gelu only)", cfg.HiddenAct)
	}
	if cfg.LNEps == 0 {
		cfg.LNEps = 1e-12
	}

	st, err := openSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, fmt.Errorf("embedder: %w", err)
	}
	defer func() { _ = st.Close() }()

	// *ForMaskedLM checkpoints nest the encoder under a prefix; plain
	// *Model checkpoints do not. Detected rather than assumed so both
	// load, and so an unexpected layout fails by name instead of
	// loading zeros.
	prefix := ""
	for _, p := range []string{"", "bert.", "roberta.", "xlm-roberta."} {
		if st.has(p + "embeddings.word_embeddings.weight") {
			prefix = p
			break
		}
	}
	if !st.has(prefix + "embeddings.word_embeddings.weight") {
		return nil, fmt.Errorf("embedder: no word embeddings found; not a BERT-family checkpoint")
	}

	m := &Model{
		cfg:    cfg,
		pool:   poolingConfig(dir, PoolMean),
		posOff: positionOffset(cfg.ModelType, cfg.PadTokenID),
	}

	get := func(name string) []float32 {
		if err != nil {
			return nil
		}
		var v []float32
		v, _, err = st.tensor(prefix + name)
		return v
	}

	m.wordEmb = get("embeddings.word_embeddings.weight")
	m.posEmb = get("embeddings.position_embeddings.weight")
	m.embLNW = get("embeddings.LayerNorm.weight")
	m.embLNB = get("embeddings.LayerNorm.bias")
	if st.has(prefix + "embeddings.token_type_embeddings.weight") {
		m.typeEmb = get("embeddings.token_type_embeddings.weight")
	}
	if err != nil {
		return nil, fmt.Errorf("embedder: load embeddings: %w", err)
	}

	// Bounded by the position table, not by config: the table is what
	// actually gets indexed, and posOff eats into it.
	m.maxSeq = len(m.posEmb)/cfg.Hidden - m.posOff
	if cfg.MaxPos > 0 && cfg.MaxPos-m.posOff < m.maxSeq {
		m.maxSeq = cfg.MaxPos - m.posOff
	}
	if m.maxSeq <= 0 {
		return nil, fmt.Errorf("embedder: position table too small for offset %d", m.posOff)
	}

	m.layers = make([]layer, cfg.Layers)
	for i := range m.layers {
		p := fmt.Sprintf("encoder.layer.%d.", i)
		l := &m.layers[i]
		l.Wq, l.Bq = get(p+"attention.self.query.weight"), get(p+"attention.self.query.bias")
		l.Wk, l.Bk = get(p+"attention.self.key.weight"), get(p+"attention.self.key.bias")
		l.Wv, l.Bv = get(p+"attention.self.value.weight"), get(p+"attention.self.value.bias")
		l.Wo, l.Bo = get(p+"attention.output.dense.weight"), get(p+"attention.output.dense.bias")
		l.AttnLNW = get(p + "attention.output.LayerNorm.weight")
		l.AttnLNB = get(p + "attention.output.LayerNorm.bias")
		l.Wi, l.Bi = get(p+"intermediate.dense.weight"), get(p+"intermediate.dense.bias")
		l.Wd, l.Bd = get(p+"output.dense.weight"), get(p+"output.dense.bias")
		l.OutLNW = get(p + "output.LayerNorm.weight")
		l.OutLNB = get(p + "output.LayerNorm.bias")
		if err != nil {
			return nil, fmt.Errorf("embedder: load layer %d: %w", i, err)
		}
	}
	return m, nil
}

// Dim is the embedding width.
func (m *Model) Dim() int { return m.cfg.Hidden }

// MaxSeq is the longest token sequence this model will process.
func (m *Model) MaxSeq() int { return m.maxSeq }

// Pooling is the reduction the checkpoint declared.
func (m *Model) Pooling() Pooling { return m.pool }

// Kernel names the matmul path this binary was built with, so an
// operator can tell whether the SIMD build is actually in use.
func Kernel() string { return kernelName }
