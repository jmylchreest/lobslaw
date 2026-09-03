package commandrisk

import "strings"

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

// IsInvisible reports the format, bidi and zero-width runes that can
// make one string display as another.
func IsInvisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F: // zero-width, LRM/RLM
		return true
	case r >= 0x202A && r <= 0x202E: // bidi embedding and override
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	case r == 0xFEFF: // BOM / zero-width no-break space
		return true
	}
	return false
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
