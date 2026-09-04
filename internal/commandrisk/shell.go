package commandrisk

import (
	"strings"
	"unicode"
)

// Shell facts the classifier and the grant-key builder both need.
//
// They live here rather than beside NormaliseCommand because the
// dependency has to run one way: internal/compute imports this package,
// never the reverse. These are small, stable statements about the shell
// itself — which words it interprets, which runes make one string
// display as another — and duplicating them is how the two readers
// would eventually disagree about what a command says.

// ReservedWords are the words the shell interprets itself rather
// than executing. Quoting one changes what runs while leaving the
// rendered token identical, so a key cannot distinguish the two.
var ReservedWords = map[string]bool{
	"time": true, "do": true, "done": true, "if": true, "then": true,
	"else": true, "elif": true, "fi": true, "case": true, "esac": true,
	"while": true, "until": true, "for": true, "in": true,
	"function": true, "select": true, "coproc": true,
}

// IsInvisible reports runes that can make one string display as
// another.
//
// The whole Cf (format) category, not a hand-listed set of ranges. The
// list this replaces named twenty-odd codepoints — the zero-width
// spaces, the bidi overrides and isolates, the BOM — and missed 155
// others that Unicode 17 also classifies as format characters. Two of
// the misses were the same trick as entries that WERE covered:
// U+061C ARABIC LETTER MARK is a bidi control exactly like the LRM and
// RLM beside it, and U+2060 WORD JOINER is zero-width exactly like the
// U+200B on the list. A command carrying either rendered identically to
// one without it and stayed grantable.
//
// A category rather than a longer list, because the longer list has the
// same defect as the short one: it is right on the day it is written.
// Cf is maintained upstream and grows with the tables, so the next
// invisible codepoint is covered before anyone here hears about it.
//
// Variation selectors are included as well. They are Mn rather than Cf
// and so not swept up by the category, but their entire purpose is to
// change how the preceding character renders without appearing
// themselves, which is this function's subject exactly. The rest of Mn
// is NOT included: combining marks are how a great deal of the world
// writes, and refusing them would reject legitimate paths rather than
// deceptive ones.
//
// Every caller fails closed on a true answer — the command becomes
// unreadable, or ungrantable, or the cwd unusable as a key — so a rune
// wrongly included here costs an extra confirmation, and one wrongly
// left out costs consent obtained by misdirection.
func IsInvisible(r rune) bool {
	if unicode.Is(unicode.Cf, r) {
		return true
	}
	// Variation selectors: VS1-16, then the supplement.
	return (r >= 0xFE00 && r <= 0xFE0F) || (r >= 0xE0100 && r <= 0xE01EF)
}

func IsEnvAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	for i, r := range tok[:eq] {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}
