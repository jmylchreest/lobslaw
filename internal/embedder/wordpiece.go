package embedder

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// The WordPiece tokenizer BERT ships.
//
// Simpler than Unigram in every respect — greedy longest-match instead
// of a Viterbi over a lattice, and no probabilities — but with its own
// normalisation, which is where the subtlety lives. BertNormalizer
// lowercases, strips accents, isolates CJK characters and scrubs
// control codes, and each of those changes which pieces match.
//
// Worth having because every small ENGLISH embedder is WordPiece:
// all-MiniLM-L6-v2 is 91 MB against multilingual-e5-small's 471 MB,
// and 82% of that difference is a multilingual vocabulary an
// English-only node never uses.

// bertNormalizer is the BertNormalizer stage, configured from
// tokenizer.json rather than assumed.
type bertNormalizer struct {
	cleanText          bool
	handleChineseChars bool
	stripAccents       bool
	lowercase          bool
}

// normalize applies the stages in the order the reference does.
//
// ORDER MATTERS and is not obvious: accents are stripped AFTER
// lowercasing, and CJK spacing happens before either. Reordering gives
// different pieces for accented and mixed-script text while leaving
// plain ASCII identical — so it looks correct on every English test.
func (b bertNormalizer) normalize(s string) string {
	if b.cleanText {
		s = cleanText(s)
	}
	if b.handleChineseChars {
		s = spaceCJK(s)
	}
	if b.lowercase {
		s = strings.ToLower(s)
	}
	if b.stripAccents {
		s = stripAccents(s)
	}
	return s
}

// cleanText drops control characters and normalises whitespace.
//
// NUL and the Unicode replacement character are removed outright, as
// the reference does; other control codes become nothing, and every
// whitespace form becomes a plain space so the pre-tokenizer has one
// separator to look for.
func cleanText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0 || r == 0xFFFD || isControl(r) {
			continue
		}
		if isWhitespace(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isControl matches the reference's definition, which deliberately
// excludes tab, newline and carriage return — those are whitespace,
// handled above, not control characters to be deleted.
func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false
	}
	return unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r)
}

func isWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return unicode.Is(unicode.Zs, r)
}

// spaceCJK puts a space either side of every CJK character.
//
// Chinese and Japanese are written without spaces, so without this the
// pre-tokenizer hands whole sentences to WordPiece as single "words"
// and almost all of them become [UNK]. Isolating each character makes
// them individually matchable — which is how BERT handles CJK at all.
func spaceCJK(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isCJK(r) {
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isCJK is the reference's block list — the CJK ideograph ranges only.
// Hiragana, katakana and hangul are deliberately NOT included: the
// reference treats them as ordinary letters.
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x20000 && r <= 0x2A6DF,
		r >= 0x2A700 && r <= 0x2B73F,
		r >= 0x2B740 && r <= 0x2B81F,
		r >= 0x2B820 && r <= 0x2CEAF,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0x2F800 && r <= 0x2FA1F:
		return true
	}
	return false
}

// stripAccents decomposes and drops the combining marks, so "café"
// becomes "cafe" and matches the vocabulary's ASCII piece.
func stripAccents(s string) string {
	decomposed := norm.NFD.String(s)
	var b strings.Builder
	b.Grow(len(decomposed))
	for _, r := range decomposed {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// bertPreTokenize splits on whitespace and isolates punctuation.
//
// Punctuation becomes its own token rather than clinging to the word,
// so "don't" is three pieces and "hello," is two. Without it every
// trailing comma would produce a different piece from the bare word.
func bertPreTokenize(s string) []string {
	var out []string
	for _, field := range strings.Fields(s) {
		var cur strings.Builder
		for _, r := range field {
			if isPunctuation(r) {
				if cur.Len() > 0 {
					out = append(out, cur.String())
					cur.Reset()
				}
				out = append(out, string(r))
				continue
			}
			cur.WriteRune(r)
		}
		if cur.Len() > 0 {
			out = append(out, cur.String())
		}
	}
	return out
}

// isPunctuation matches the reference, which counts every ASCII
// non-alphanumeric printable as punctuation — including $, + and ` —
// on top of the Unicode punctuation categories.
func isPunctuation(r rune) bool {
	if (r >= 33 && r <= 47) || (r >= 58 && r <= 64) ||
		(r >= 91 && r <= 96) || (r >= 123 && r <= 126) {
		return true
	}
	return unicode.IsPunct(r)
}

// wordPiece is the greedy longest-match segmentation.
type wordPiece struct {
	vocab        map[string]int32
	unkID        int32
	prefix       string
	maxWordLen   int
	norm         bertNormalizer
	clsID, sepID int32
}

// encode tokenises one pre-tokenized word.
//
// Greedy longest-match FROM THE LEFT, with the continuation prefix on
// every piece after the first. If any position has no match at all the
// WHOLE word becomes [UNK] — not the matched prefix plus [UNK] for the
// rest, which is the natural mistake and produces plausible ids that
// disagree with the reference.
func (w *wordPiece) encodeWord(word string) []int32 {
	if len([]rune(word)) > w.maxWordLen {
		return []int32{w.unkID}
	}
	var out []int32
	start := 0
	for start < len(word) {
		end := len(word)
		var found string
		matched := false
		for end > start {
			sub := word[start:end]
			if start > 0 {
				sub = w.prefix + sub
			}
			if _, ok := w.vocab[sub]; ok {
				found = sub
				matched = true
				break
			}
			// Back off one RUNE, not one byte: slicing mid-rune would
			// look up invalid UTF-8 and never match.
			_, size := decodeLastRune(word[start:end])
			end -= size
		}
		if !matched {
			return []int32{w.unkID}
		}
		out = append(out, w.vocab[found])
		start += len(found)
		if start > 0 && strings.HasPrefix(found, w.prefix) {
			start -= len(w.prefix)
		}
	}
	return out
}

func decodeLastRune(s string) (rune, int) {
	if s == "" {
		return 0, 1
	}
	r := []rune(s)
	last := r[len(r)-1]
	return last, len(string(last))
}
