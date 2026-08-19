// Package textutil holds string handling that must be right about
// characters rather than bytes.
package textutil

import (
	"strings"
	"unicode/utf8"
)

// Truncating a string is not slicing it.
//
// A Telegram paste containing em dashes and arrows was cut at
// userMsg[:140], byte 140 landed inside a multi-byte character, and
// protobuf refused to marshal the record:
//
//	agent: episodic ingest failed; turn still succeeded
//	err="marshal: string field contains invalid UTF-8"
//
// The turn answered normally and nothing was remembered about it. The
// failure is silent by construction — the ingest is a background
// goroutine whose error is logged and dropped, because a memory write
// must not fail somebody's answer.
//
// Seven places in this tree sliced strings by byte offset to shorten
// them. Every one could produce invalid UTF-8 from ordinary prose, and
// they fed protobuf, prompts, and Telegram.

// Truncate shortens s to at most max RUNES, appending ellipsis when it
// had to cut.
//
// Counted in runes rather than bytes because the caller means
// "roughly this much text". A byte limit gives a Japanese speaker a
// third of what it gives an English one, and cuts a character in half
// at the boundary.
//
// max <= 0 returns the empty string: a caller asking for no characters
// gets none, rather than the ellipsis alone.
func Truncate(s, ellipsis string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	// Ranging a string yields rune boundaries, so the cut can never
	// land inside a character.
	count := 0
	for i := range s {
		if count == max {
			return s[:i] + ellipsis
		}
		count++
	}
	return s
}

// Sanitise replaces any invalid UTF-8 with the replacement character.
//
// Belt to Truncate's braces, for text this process did not cut: a
// provider, a fetched page or a channel can hand us bytes that are not
// valid UTF-8, and the first thing that notices is a protobuf marshal
// three layers away from the cause.
//
// Cheap when the string is already valid, which is the common case —
// ToValidUTF8 returns the original.
func Sanitise(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}
