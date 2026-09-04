package commandrisk

import "testing"

// The codepoints the hand-written range list missed.
//
// U+061C and U+2060 are the ones that mattered: each is the same trick
// as a codepoint the old list DID cover, sitting one range away from it.
func TestIsInvisibleCoversFormatCharacters(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]rune{
		"ARABIC LETTER MARK (bidi, like LRM/RLM)": 0x061C,
		"WORD JOINER (zero-width, like ZWSP)":     0x2060,
		"SOFT HYPHEN":                             0x00AD,
		"MONGOLIAN VOWEL SEPARATOR":               0x180E,
		"INVISIBLE TIMES":                         0x2062,
		"INVISIBLE SEPARATOR":                     0x2063,
		"INHIBIT SYMMETRIC SWAPPING":              0x206A,
		"INTERLINEAR ANNOTATION ANCHOR":           0xFFF9,
		"ARABIC NUMBER SIGN":                      0x0600,
		"LANGUAGE TAG":                            0xE0001,
		"VARIATION SELECTOR-16":                   0xFE0F,
		// Already covered before; must stay covered.
		"ZERO WIDTH SPACE":       0x200B,
		"RIGHT-TO-LEFT OVERRIDE": 0x202E,
		"BYTE ORDER MARK":        0xFEFF,
		"LEFT-TO-RIGHT ISOLATE":  0x2066,
	} {
		if !IsInvisible(r) {
			t.Errorf("IsInvisible(U+%04X) = false — %s can make one command display as another", r, name)
		}
	}
}

// Refusing every combining mark would reject the scripts a great deal
// of the world writes in, which is a different failure from the one
// this guards against.
func TestIsInvisibleLeavesVisibleTextAlone(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]rune{
		"latin a":           'a',
		"space":             ' ',
		"slash":             '/',
		"hyphen":            '-',
		"e-acute":           'é',
		"combining acute":   0x0301, // Mn, but visible as a mark
		"arabic letter beh": 0x0628,
		"han character":     0x4E2D,
		"emoji (no VS)":     0x1F600,
	} {
		if IsInvisible(r) {
			t.Errorf("IsInvisible(U+%04X) = true — %s is visible text", r, name)
		}
	}
}

// End to end: the reason any of this matters.
//
// "git<U+2060>status" renders as "gitstatus" in every prompt that quotes
// it back. Before the category widened, the word joiner was not on the
// list, the segment scanned clean, and the command was classified on
// the strength of a program name the user could not see.
func TestWordJoinerMakesACommandUnreadable(t *testing.T) {
	t.Parallel()
	got := ClassifyRisk("git\u2060status")
	if !HasLabel(got.Labels, LabelUnreadable) {
		t.Errorf("ClassifyRisk = %v, want unreadable", got.Labels)
	}
	// The visible spelling still reads normally, so this is not the
	// tokeniser refusing anything unusual.
	if plain := ClassifyRisk("git status"); !HasLabel(plain.Labels, LabelReads) {
		t.Errorf("plain `git status` = %v, want reads", plain.Labels)
	}
}
