package embedder

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
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
	replaces []replaceRule
	// whitespaceSplit is whether the pre-tokenizer splits on
	// whitespace BEFORE applying Metaspace.
	//
	// The two checkpoints in the fixture set disagree, which is why
	// this is read from the file rather than assumed:
	//
	//	e5-base   Sequence[WhitespaceSplit, Metaspace]
	//	e5-small  Metaspace
	//
	// It is not cosmetic. With the split, each word is a separate
	// Viterbi over "▁word". Without it, the whole normalised string
	// becomes one "▁hello▁world" and the lattice runs across the lot,
	// so a piece may span what used to be a word boundary. Assuming
	// either one produces valid-looking ids for the other model.
	whitespaceSplit bool
	addPrefixSpace  bool
	piece2id        map[string]int32
	scores          []float64
	maxBytes        int
	minScore        float64
	unkID           int32
	bosID           int32
	eosID           int32
}

// tokenizerJSON is the subset of tokenizer.json this needs.
// replaceRule is a normalizer Replace step: a regex and what it
// becomes. e5-small collapses runs of spaces this way.
type replaceRule struct {
	re      *regexp.Regexp
	content string
}

type normalizerJSON struct {
	Type                string           `json:"type"`
	PrecompiledCharsmap string           `json:"precompiled_charsmap"`
	Normalizers         []normalizerJSON `json:"normalizers"`
	Pattern             struct {
		Regex  string `json:"Regex"`
		String string `json:"String"`
	} `json:"pattern"`
	Content string `json:"content"`
}

type preTokenizerJSON struct {
	Type           string             `json:"type"`
	PreTokenizers  []preTokenizerJSON `json:"pretokenizers"`
	AddPrefixSpace *bool              `json:"add_prefix_space"`
}

type tokenizerJSON struct {
	Normalizer   normalizerJSON   `json:"normalizer"`
	PreTokenizer preTokenizerJSON `json:"pre_tokenizer"`
	Model        struct {
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
		// Named explicitly, because WordPiece is the common case and
		// "unsupported" alone sends people looking for a bug. Most
		// English BERT embedders — bge-small-en, all-MiniLM, e5-base-v2
		// — are WordPiece and will land here. The forward pass would
		// run them; the tokenizer is what does not.
		return nil, fmt.Errorf("embedder: tokenizer model %q unsupported — this reads SentencePiece Unigram "+
			"(XLM-RoBERTa family: multilingual-e5-*, bge-m3), not WordPiece", meta.Model.Type)
	}
	charsmap, replaces, err := flattenNormalizer(meta.Normalizer)
	if err != nil {
		return nil, fmt.Errorf("embedder: %w", err)
	}
	whitespaceSplit, addPrefixSpace, err := readPreTokenizer(meta.PreTokenizer)
	if err != nil {
		return nil, fmt.Errorf("embedder: %w", err)
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

	norm, err := newPrecompiled(charsmap)
	if err != nil {
		return nil, fmt.Errorf("embedder: %w", err)
	}

	t := &Tokenizer{
		norm:            norm,
		replaces:        replaces,
		whitespaceSplit: whitespaceSplit,
		addPrefixSpace:  addPrefixSpace,
		piece2id:        make(map[string]int32, len(entries)),
		scores:          make([]float64, len(entries)),
		minScore:        math.Inf(1),
		unkID:           3,
		bosID:           -1,
		eosID:           -1,
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
	for _, r := range t.replaces {
		normalized = r.re.ReplaceAllString(normalized, r.content)
	}

	if t.whitespaceSplit {
		// Each whitespace-delimited word tokenised independently with a
		// leading U+2581, so no piece can span a word boundary.
		var out []int32
		for _, word := range strings.Fields(normalized) {
			out = append(out, t.viterbi(metaspace+word)...)
		}
		return out
	}

	// Bare Metaspace: spaces BECOME the marker and the whole string is
	// one lattice.
	//
	// NOT trimmed. An earlier version trimmed first, reasoning that a
	// trailing space would produce a lone "▁" piece — and the
	// reference emits exactly that piece. "  leading and trailing  "
	// ends in id 6, the bare marker. Trimming dropped it and made this
	// the only failing case out of 119.
	if normalized == "" {
		return nil
	}
	replaced := strings.ReplaceAll(normalized, " ", metaspace)
	if t.addPrefixSpace && !strings.HasPrefix(replaced, metaspace) {
		replaced = metaspace + replaced
	}
	return t.viterbi(replaced)
}

// flattenNormalizer accepts a bare Precompiled or a Sequence containing
// one, returning the charsmap and any Replace steps that follow it.
func flattenNormalizer(n normalizerJSON) (string, []replaceRule, error) {
	switch n.Type {
	case "Precompiled":
		return n.PrecompiledCharsmap, nil, nil
	case "Sequence":
		var charsmap string
		var rules []replaceRule
		for _, sub := range n.Normalizers {
			switch sub.Type {
			case "Precompiled":
				charsmap = sub.PrecompiledCharsmap
			case "Replace":
				pattern := sub.Pattern.Regex
				if pattern == "" {
					// A literal String pattern, quoted so its
					// punctuation is not read as a regex.
					pattern = regexp.QuoteMeta(sub.Pattern.String)
				}
				re, err := regexp.Compile(pattern)
				if err != nil {
					return "", nil, fmt.Errorf("normalizer Replace pattern %q: %w", pattern, err)
				}
				rules = append(rules, replaceRule{re: re, content: sub.Content})
			default:
				// Refused rather than skipped. A normalizer step this
				// package silently ignored would produce ids that look
				// right and are not, which is the one failure mode a
				// tokenizer must never have.
				return "", nil, fmt.Errorf("normalizer step %q unsupported", sub.Type)
			}
		}
		if charsmap == "" {
			return "", nil, fmt.Errorf("normalizer Sequence contains no Precompiled step")
		}
		return charsmap, rules, nil
	default:
		return "", nil, fmt.Errorf("normalizer %q unsupported (Precompiled, or a Sequence containing one)", n.Type)
	}
}

// readPreTokenizer reports whether whitespace is split before Metaspace,
// and whether Metaspace adds its leading marker.
func readPreTokenizer(p preTokenizerJSON) (whitespaceSplit, addPrefixSpace bool, err error) {
	addPrefixSpace = true
	apply := func(sub preTokenizerJSON) error {
		switch sub.Type {
		case "WhitespaceSplit":
			whitespaceSplit = true
		case "Metaspace":
			if sub.AddPrefixSpace != nil {
				addPrefixSpace = *sub.AddPrefixSpace
			}
		default:
			return fmt.Errorf("pre-tokenizer step %q unsupported", sub.Type)
		}
		return nil
	}
	if p.Type == "Sequence" {
		for _, sub := range p.PreTokenizers {
			if err := apply(sub); err != nil {
				return false, false, err
			}
		}
		return whitespaceSplit, addPrefixSpace, nil
	}
	if err := apply(p); err != nil {
		return false, false, err
	}
	return whitespaceSplit, addPrefixSpace, nil
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
