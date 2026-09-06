package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

// WHY THE LEXICAL PATH SCORED GREETINGS PERFECTLY.
//
// Two defects, and neither was reachable by tuning the floor:
//
//   - Matching was strings.Contains, which matches inside a longer
//     word. "hey" matched "they", so "Hey you there?" scored 1.0
//     against any record containing an ordinary English "they".
//   - The score is matched terms over total terms, so a query that
//     survives tokenisation as ONE term is quantised to {0, 1}. A
//     single word appearing somewhere produced the maximum score,
//     indistinguishable from a fully matched query.
//
// The first is fixed for every caller, because it is a bug. The second
// is a rule applied to passive recall only, because searching for one
// word explicitly is a reasonable thing to ask for.

func TestMatchesAtWordStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		hay, tok string
		want     bool
	}{
		// The actual incident: the greeting's only surviving term
		// against ordinary prose.
		{"they had already been bought", "hey", false},
		{"expenses are refused unless they are filed", "hey", false},

		{"hey there, what's the plan", "hey", true},
		{"the sourdough starter is fed on tuesdays", "sourdough", true},

		// Prefixes still match, deliberately: there is no stemmer
		// here, and losing "run" -> "running" to fix "hey" -> "they"
		// would trade one class of miss for another.
		{"john has been running three times a week", "run", true},
		{"the boiler service contract renews", "renew", true},

		// A suffix or interior match is exactly what was wrong.
		{"the boiler service contract renews", "news", false},
		{"tinned new potatoes", "potato", true},
		{"tinned new potatoes", "otato", false},

		{"cattery needs booking", "cat", true},
		{"the concatenation is wrong", "cat", false},

		// Word starts after punctuation and digits, not only spaces.
		{"bought oat-bars yesterday", "bars", true},
		{"item 752 was ticked off", "752", true},

		// A multi-byte letter before the match is still a letter, so
		// this must not be read as a boundary.
		{"caféhey is not a word", "hey", false},

		{"", "hey", false},
		{"short", "muchlongertoken", false},
	} {
		if got := matchesAtWordStart(tc.hay, tc.tok); got != tc.want {
			t.Errorf("matchesAtWordStart(%q, %q) = %v, want %v", tc.hay, tc.tok, got, tc.want)
		}
	}
}

func TestTokeniseQueryReducesContractions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		query string
		want  []string
	}{
		// "what" is a stopword; "what's" was not, so the identical
		// question kept a term depending on how it was typed.
		{"what's new?", []string{"new"}},
		{"what is new?", []string{"new"}},
		// A possessive reduces to the word worth searching for.
		{"john's shopping list", []string{"john", "shopping", "list"}},
		// n't likewise, and "does" is not in the list so it survives.
		{"doesn't matter", []string{"does", "matter"}},
		// Not a contraction; must not be mangled.
		{"the boss stopped", []string{"boss", "stopped"}},
	} {
		got := TokeniseQuery(tc.query)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("TokeniseQuery(%q) = %v, want %v", tc.query, got, tc.want)
		}
	}
}

// A greeting reaches the scorer as one term and would score 1.0. The
// rule is checked before the scan, so it costs no bucket walk either.
func TestPassiveLexicalRecallDeclinesSingleTermQueries(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	// Contains "they", which the old substring match found "hey" in.
	seedEpisodicOnly(t, store, "e1",
		"added galaxy chocolate to the list because they asked for it", nil)

	e := NewContextEngine(ContextEngineConfig{Store: store})
	for _, greeting := range []string{"Hey you there?", "morning", "still there?", "you around?"} {
		if got := e.Assemble(operatorTurn(context.Background()), greeting).Rendered(); got != "" {
			t.Errorf("the greeting %q recalled something:\n%s", greeting, got)
		}
	}

	// The same corpus is still reachable by a real question, so the
	// rule is declining short queries rather than breaking recall.
	got := e.Assemble(operatorTurn(context.Background()), "was the galaxy chocolate added").Rendered()
	if !strings.Contains(got, "chocolate") {
		t.Errorf("a real question recalled nothing:\n%s", got)
	}
}

// THE PART THAT MUST NOT REGRESS.
//
// The minimum-term rule is a policy about UNINVITED recall. memory_search
// is an explicit request, and asking it for a single word is entirely
// reasonable — so it runs the same scan without the rule. If this ever
// starts failing, the rule has leaked out of the passive path and the
// agent has lost the ability to look something up by name.
func TestExplicitSearchStillAcceptsASingleTerm(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodicOnly(t, store, "e1", "the sourdough starter is fed on tuesdays", nil)

	payload, _, err := RunSubstringSearch(store, memory.Everyone(), "sourdough", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "sourdough") {
		t.Errorf("a one-word explicit search found nothing:\n%s", payload)
	}
}

// The word-boundary fix applies to the explicit path too, because it
// is a bug rather than a policy: "hey" was never a match for "they" in
// either direction.
func TestExplicitSearchDoesNotMatchInsideWords(t *testing.T) {
	t.Parallel()
	store := newMemoryStoreForTest(t)
	seedEpisodicOnly(t, store, "e1", "they had already been bought", nil)

	payload, _, err := RunSubstringSearch(store, memory.Everyone(), "hey bought", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	// "bought" is a real match, so the record is returned — what
	// matters is that it is not returned on the strength of "hey".
	if !strings.Contains(string(payload), "bought") {
		t.Fatalf("the record should still match on 'bought':\n%s", payload)
	}
	if matchesAtWordStart("they had already been bought", "hey") {
		t.Error("'hey' still matches inside 'they'")
	}
}
