package embedder

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SentencePiece's Precompiled normalizer.
//
// XLM-R's tokenizer.json carries a 316 KB base64 blob called
// precompiled_charsmap. It is not NFKC, though it is close: a
// SentencePiece-specific table baked into a Darts double-array trie
// mapping byte sequences to their replacements.
//
// Approximating it with x/text's NFKC was tempting and would have been
// wrong in a way nothing would report. The mapping differs from NFKC in
// enough places that some inputs would tokenise to different ids, which
// means different embeddings, which means recall that is quietly worse
// for exactly the text the difference touches — accented Latin,
// full-width CJK punctuation, and the compatibility characters people
// paste out of documents.
//
// FORMAT (from sentencepiece's normalizer.cc):
//
//	[0:4]              trie size in bytes, little-endian
//	[4:4+trieSize]     Darts double-array, one uint32 per node
//	[4+trieSize:]      NUL-separated replacement strings
//
// A trie hit yields an offset into that final blob; the replacement is
// the NUL-terminated string there.
type precompiled struct {
	array []uint32
	pool  []byte
}

// The Darts-clone unit encoding. These are bit-layout facts about the
// container, not choices — see darts.h in sentencepiece.
func dartsHasLeaf(u uint32) bool { return (u>>8)&1 == 1 }
func dartsValue(u uint32) uint32 { return u & 0x7fffffff }
func dartsLabel(u uint32) uint32 { return u & (1<<31 | 0xff) }
func dartsOffset(u uint32) uint32 {
	return (u >> 10) << ((u & (1 << 9)) >> 6)
}

func newPrecompiled(b64 string) (*precompiled, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("precompiled_charsmap: base64: %w", err)
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("precompiled_charsmap: %d bytes, too short to hold a trie size", len(raw))
	}
	size := int(binary.LittleEndian.Uint32(raw[:4]))
	// Bounded before it is used to size an allocation: the blob comes
	// from a downloaded file.
	if size < 0 || size%4 != 0 || 4+size > len(raw) {
		return nil, fmt.Errorf("precompiled_charsmap: trie size %d is invalid for a %d byte blob", size, len(raw))
	}
	array := make([]uint32, size/4)
	for i := range array {
		array[i] = binary.LittleEndian.Uint32(raw[4+i*4:])
	}
	return &precompiled{array: array, pool: raw[4+size:]}, nil
}

// lookup walks the trie for the longest prefix of key that has a
// replacement.
func (p *precompiled) lookup(key []byte) (uint32, bool) {
	if len(p.array) == 0 {
		return 0, false
	}
	pos := int(dartsOffset(p.array[0]))
	for _, c := range key {
		// A NUL terminates a key by construction; continuing past it
		// would walk into an unrelated branch.
		if c == 0 {
			break
		}
		pos ^= int(c)
		if pos < 0 || pos >= len(p.array) {
			return 0, false
		}
		unit := p.array[pos]
		if dartsLabel(unit) != uint32(c) {
			return 0, false
		}
		pos ^= int(dartsOffset(unit))
		if dartsHasLeaf(unit) {
			if pos < 0 || pos >= len(p.array) {
				return 0, false
			}
			return dartsValue(p.array[pos]), true
		}
	}
	return 0, false
}

// replacement returns the mapped string for an exact byte sequence.
func (p *precompiled) replacement(chunk []byte) (string, bool) {
	idx, ok := p.lookup(chunk)
	if !ok {
		return "", false
	}
	if int(idx) >= len(p.pool) {
		// A trie that points outside its own pool is corrupt, but
		// mapping to empty is what sentencepiece does rather than
		// failing the whole tokenisation.
		return "", true
	}
	rest := p.pool[idx:]
	if end := indexZero(rest); end >= 0 {
		return string(rest[:end]), true
	}
	return string(rest), true
}

func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// isCombining reports whether r is a mark that attaches to the
// preceding character.
func isCombining(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r)
}

// normalize applies the charsmap to a whole string.
//
// GRAPHEME CLUSTER FIRST, then per-rune. The table contains multi-rune
// entries — a base letter plus its combining accent mapping to a single
// precomposed character — so looking up one rune at a time would miss
// them and leave "cafe" + U+0301 as two tokens where the model expects
// one. That is exactly the NFD fixture in the golden set.
//
// The 6-byte guard mirrors sentencepiece: clusters longer than that are
// not in the table, so the lookup would be wasted work on every long
// run of marks.
func (p *precompiled) normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		if size == 0 {
			size = 1
		}
		end := i + size
		for end < len(s) {
			r, sz := utf8.DecodeRuneInString(s[end:])
			if !isCombining(r) {
				break
			}
			end += sz
		}
		cluster := s[i:end]
		i = end

		if len(cluster) < 6 {
			if rep, ok := p.replacement([]byte(cluster)); ok {
				b.WriteString(rep)
				continue
			}
		}
		for j := 0; j < len(cluster); {
			_, sz := utf8.DecodeRuneInString(cluster[j:])
			if sz == 0 {
				sz = 1
			}
			part := cluster[j : j+sz]
			if rep, ok := p.replacement([]byte(part)); ok {
				b.WriteString(rep)
			} else {
				b.WriteString(part)
			}
			j += sz
		}
	}
	return b.String()
}
