package embedder

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf8"
)

// The SentencePiece Unigram tokenizer XLM-RoBERTa ships.
//
// Text becomes ids in four stages, and every one of them is a place a
// near-miss produces plausible-but-wrong output rather than an error:
//
//	normalize     the Precompiled charsmap (see charsmap.go)
//	pre-tokenize  split on whitespace, then prefix each word with U+2581
//	Viterbi       best-scoring segmentation over the vocabulary
//	post-process  wrap as <s> ... </s>
//
// The golden fixtures pin the result as EXACT INTEGERS, which is the
// only useful tolerance for a tokenizer: ids either match the reference
// or they do not, and a tokenizer that is 99% right degrades every
// embedding it produces without ever failing.

// unkPenalty is K_UNK_PENALTY from sentencepiece's unigram_model.cc.
// An unknown piece scores minScore - 10, making it a last resort the
// Viterbi will route around wherever any real segmentation exists.
const unkPenalty = 10.0

// metaspace is U+2581 LOWER ONE EIGHTH BLOCK, SentencePiece's stand-in
// for a space. Word boundaries have to survive into the vocabulary, so
// "▁the" and "the" are different pieces — the first only matches at the
// start of a word.
const metaspace = "▁"

// Tokenizer turns text into the ids a Model expects.
//
// Immutable after Load and safe for concurrent use: every field is
// read-only and the Viterbi allocates its own working state per call.
type Tokenizer struct {
	norm     *precompiled
	piece2id map[string]int32
	scores   []float64
	maxBytes int
	minScore float64
	unkID    int32
	bosID    int32
	eosID    int32
}

// tokenizerJSON is the subset of tokenizer.json this needs.
type tokenizerJSON struct {
	Normalizer struct {
		Type                string `json:"type"`
		PrecompiledCharsmap string `json:"precompiled_charsmap"`
	} `json:"normalizer"`
	Model struct {
		Type  string          `json:"type"`
		UnkID *int            `json:"unk_id"`
		Vocab [][]json.Number `json:"-"`
	} `json:"model"`
}

// LoadTokenizer reads a HuggingFace tokenizer.json.
func LoadTokenizer(path string) (*Tokenizer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("embedder: read tokenizer.json: %w", err)
	}
	var meta tokenizerJSON
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("embedder: parse tokenizer.json: %w", err)
	}
	if meta.Model.Type != "Unigram" {
		return nil, fmt.Errorf("embedder: tokenizer model %q unsupported (Unigram only)", meta.Model.Type)
	}
	if meta.Normalizer.Type != "Precompiled" {
		return nil, fmt.Errorf("embedder: normalizer %q unsupported (Precompiled only)", meta.Normalizer.Type)
	}

	// The vocabulary is [["piece", logprob], ...] with mixed types, so
	// it is decoded separately rather than fought with struct tags.
	var vocabWrap struct {
		Model struct {
			Vocab []json.RawMessage `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(raw, &vocabWrap); err != nil {
		return nil, fmt.Errorf("embedder: parse vocab: %w", err)
	}
	entries := vocabWrap.Model.Vocab
	if len(entries) == 0 {
		return nil, fmt.Errorf("embedder: tokenizer.json has an empty vocabulary")
	}

	norm, err := newPrecompiled(meta.Normalizer.PrecompiledCharsmap)
	if err != nil {
		return nil, fmt.Errorf("embedder: %w", err)
	}

	t := &Tokenizer{
		norm:     norm,
		piece2id: make(map[string]int32, len(entries)),
		scores:   make([]float64, len(entries)),
		minScore: math.Inf(1),
		unkID:    3,
		bosID:    -1,
		eosID:    -1,
	}
	if meta.Model.UnkID != nil {
		t.unkID = int32(*meta.Model.UnkID)
	}
	for i, e := range entries {
		var pair []json.RawMessage
		if err := json.Unmarshal(e, &pair); err != nil || len(pair) != 2 {
			return nil, fmt.Errorf("embedder: vocab entry %d is not [piece, score]", i)
		}
		var piece string
		if err := json.Unmarshal(pair[0], &piece); err != nil {
			return nil, fmt.Errorf("embedder: vocab entry %d has a non-string piece", i)
		}
		var score float64
		if err := json.Unmarshal(pair[1], &score); err != nil {
			return nil, fmt.Errorf("embedder: vocab entry %d has a non-numeric score", i)
		}
		id := int32(i)
		// FIRST occurrence wins. A duplicate piece later in the file
		// must not silently steal the id, because the reference
		// implementation keeps the first too.
		if _, seen := t.piece2id[piece]; !seen {
			t.piece2id[piece] = id
		}
		t.scores[i] = score
		if len(piece) > t.maxBytes {
			t.maxBytes = len(piece)
		}
		if score < t.minScore {
			t.minScore = score
		}
		switch piece {
		case "<s>":
			t.bosID = id
		case "</s>":
			t.eosID = id
		}
	}
	return t, nil
}

// BOS and EOS are the boundary ids, for callers that chunk long input.
func (t *Tokenizer) BOS() int32 { return t.bosID }
func (t *Tokenizer) EOS() int32 { return t.eosID }

// Encode returns the token ids for text, WITHOUT special tokens.
func (t *Tokenizer) Encode(text string) []int32 {
	normalized := t.norm.normalize(text)
	var out []int32
	// WhitespaceSplit then Metaspace: each whitespace-delimited word is
	// tokenised independently with a leading U+2581. Running Viterbi
	// over the whole string at once would let a piece span a word
	// boundary, which the vocabulary was not built for.
	for _, word := range strings.Fields(normalized) {
		out = append(out, t.viterbi(metaspace+word)...)
	}
	return out
}

// EncodeWithSpecials returns ids wrapped as the model expects and
// truncated to maxLen INCLUDING the specials.
//
// Truncating before wrapping would produce a sequence one or two
// tokens over the model's limit, which hiddenStates would then cut —
// silently dropping the closing token the model was trained to expect.
func (t *Tokenizer) EncodeWithSpecials(text string, maxLen int) []int32 {
	ids := t.Encode(text)
	room := maxLen
	if t.bosID >= 0 {
		room--
	}
	if t.eosID >= 0 {
		room--
	}
	if room < 0 {
		room = 0
	}
	if len(ids) > room {
		ids = ids[:room]
	}
	out := make([]int32, 0, len(ids)+2)
	if t.bosID >= 0 {
		out = append(out, t.bosID)
	}
	out = append(out, ids...)
	if t.eosID >= 0 {
		out = append(out, t.eosID)
	}
	return out
}

// viterbi finds the highest-scoring segmentation of one pre-tokenized
// word.
//
// A byte-position dynamic program: best[i] is the best score for the
// prefix ending at byte i, and each vocabulary piece matching at a
// position offers an edge. Greedy longest-match would be simpler and
// wrong — Unigram's whole premise is that the best segmentation is not
// always the one with the longest first piece.
func (t *Tokenizer) viterbi(word string) []int32 {
	size := len(word)
	if size == 0 {
		return nil
	}
	unkScore := t.minScore - unkPenalty

	type node struct {
		id      int32
		score   float64
		startAt int
		reached bool
	}
	best := make([]node, size+1)
	best[0].reached = true

	for start := 0; start < size; {
		_, runeLen := utf8.DecodeRuneInString(word[start:])
		if runeLen == 0 {
			runeLen = 1
		}
		if best[start].reached {
			till := best[start].score
			hasSingleRune := false
			maxEnd := min(start+t.maxBytes, size)
			for end := start + 1; end <= maxEnd; end++ {
				id, ok := t.piece2id[word[start:end]]
				if !ok {
					continue
				}
				cand := t.scores[id] + till
				n := &best[end]
				if !n.reached || cand > n.score {
					*n = node{id: id, score: cand, startAt: start, reached: true}
				}
				if end-start == runeLen {
					hasSingleRune = true
				}
			}
			// Every position must have SOME outgoing edge or the
			// lattice breaks and the backtrack cannot reach the start.
			// A rune absent from the vocabulary gets one to <unk>.
			if !hasSingleRune {
				n := &best[start+runeLen]
				cand := unkScore + till
				if !n.reached || cand > n.score {
					*n = node{id: t.unkID, score: cand, startAt: start, reached: true}
				}
			}
		}
		start += runeLen
	}

	var reversed []int32
	for end := size; end > 0; {
		n := best[end]
		if !n.reached {
			break
		}
		reversed = append(reversed, n.id)
		end = n.startAt
	}
	out := make([]int32, 0, len(reversed))
	// fuse_unk: consecutive unknown pieces collapse into a single
	// <unk>, matching the reference. Without it a run of unmapped
	// characters becomes one id per character.
	for i := len(reversed) - 1; i >= 0; i-- {
		id := reversed[i]
		if id == t.unkID && len(out) > 0 && out[len(out)-1] == t.unkID {
			continue
		}
		out = append(out, id)
	}
	return out
}
