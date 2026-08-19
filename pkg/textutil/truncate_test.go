package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// THE BUG. A paste containing em dashes was cut at byte 140, the cut
// landed inside a character, and protobuf refused the record — so the
// turn answered and nothing was remembered about it.
func TestACutNeverLandsInsideACharacter(t *testing.T) {
	t.Parallel()
	// Every one of these is multi-byte, so a byte-offset cut through
	// the middle of the string is guaranteed to split one.
	for _, s := range []string{
		strings.Repeat("—", 100),
		strings.Repeat("日本語", 100),
		strings.Repeat("🦞", 100),
		strings.Repeat("café ", 100),
	} {
		for _, max := range []int{1, 7, 40, 139, 140, 141} {
			got := Truncate(s, "…", max)
			if !utf8.ValidString(got) {
				t.Fatalf("Truncate(%.10q…, %d) produced invalid UTF-8", s, max)
			}
		}
	}
}

// The limit is in characters, because the caller means "roughly this
// much text". A byte limit gives a Japanese speaker a third of what it
// gives an English one.
func TestTheLimitIsInCharactersNotBytes(t *testing.T) {
	t.Parallel()
	s := strings.Repeat("日", 10) // 30 bytes, 10 runes
	got := Truncate(s, "", 10)
	if got != s {
		t.Errorf("a 10-rune string was truncated at a 10-rune limit: %q", got)
	}
	if got := utf8.RuneCountInString(Truncate(s, "", 4)); got != 4 {
		t.Errorf("kept %d runes, want 4", got)
	}
}

// Short strings come back untouched, ellipsis and all — appending one
// to text that was not cut says something happened that did not.
func TestNothingIsAppendedWhenNothingWasCut(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "short", strings.Repeat("a", 140)} {
		if got := Truncate(s, "…", 140); got != s {
			t.Errorf("Truncate(%q) = %q, want it unchanged", s, got)
		}
	}
}

func TestTheEllipsisIsAppendedWhenItWasCut(t *testing.T) {
	t.Parallel()
	got := Truncate(strings.Repeat("a", 200), "…", 140)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("no ellipsis on a truncated string: %q", got[len(got)-5:])
	}
	if n := utf8.RuneCountInString(got); n != 141 {
		t.Errorf("kept %d runes, want 140 plus the ellipsis", n)
	}
}

// A caller asking for no characters gets none, not a bare ellipsis.
func TestZeroMeansEmpty(t *testing.T) {
	t.Parallel()
	for _, max := range []int{0, -1} {
		if got := Truncate("anything", "…", max); got != "" {
			t.Errorf("Truncate(max=%d) = %q, want empty", max, got)
		}
	}
}

// Text this process did not cut can still arrive invalid — from a
// provider, a fetched page, or a channel — and the first thing that
// notices is a marshal three layers from the cause.
func TestSanitiseRepairsWhatWeDidNotBreak(t *testing.T) {
	t.Parallel()
	broken := "hello " + string([]byte{0xff, 0xfe}) + " world"
	if utf8.ValidString(broken) {
		t.Fatal("the fixture is not actually invalid")
	}
	got := Sanitise(broken)
	if !utf8.ValidString(got) {
		t.Error("Sanitise left invalid UTF-8 behind")
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("Sanitise discarded the readable text: %q", got)
	}
	// Valid input is returned as-is, which is the common case.
	if got := Sanitise("already fine"); got != "already fine" {
		t.Errorf("Sanitise altered valid text: %q", got)
	}
}
