package embedder

import (
	"math"
	"path/filepath"
	"testing"
)

func cos(a, b []float32) float64 {
	var d, na, nb float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return d / (math.Sqrt(na) * math.Sqrt(nb))
}

// END TO END: text in, a vector that actually ranks memories.
//
// Every other test in this package checks a stage. This checks the
// thing the stages exist for — that "where do I live" retrieves
// "based in Yorkshire" over unrelated records, which is precisely what
// lexical matching cannot do because they share no words.
func TestSemanticRecallBeatsTheDistractors(t *testing.T) {
	e, err := Open(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()

	for _, tc := range []struct {
		query, want string
		distractors []string
	}{
		{"where do I live", "the user is based in Yorkshire",
			[]string{"the sourdough starter is fed on tuesdays", "the raid array uses six disks"}},
		{"what music am I into", "he listens mostly to drum and bass",
			[]string{"the backup runs at 3am", "prefers TOML over YAML"}},
		{"am I allergic to anything", "reacts badly to shellfish",
			[]string{"drives a blue estate", "commutes by bicycle"}},
		{"how do I get to work", "commutes by bicycle every morning",
			[]string{"prefers TOML over YAML", "reacts badly to shellfish"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			q := e.Encode(tc.query)
			best := cos(q, e.Encode(tc.want))
			for _, d := range tc.distractors {
				if got := cos(q, e.Encode(d)); got >= best {
					t.Errorf("distractor %q scored %.4f, above the answer %q at %.4f", d, got, tc.want, best)
				}
			}
		})
	}
}

// CROSS-LINGUAL. The reason a multilingual checkpoint was chosen: a
// memory recorded in one language must be retrievable by a question
// asked in another.
func TestAMemoryIsFoundAcrossLanguages(t *testing.T) {
	e, err := Open(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()

	en := e.Encode("the user is allergic to shellfish")
	unrelated := cos(en, e.Encode("the raid array uses six disks"))
	// nolint:misspell // The linter reads the German "allergisch" as a
	// misspelling of "allergic". It is the actual translation, and it
	// is the data under test — a multilingual encoder tested only in
	// English would prove nothing.
	for lang, text := range map[string]string{
		"french":   "l'utilisateur est allergique aux fruits de mer",
		"german":   "der Benutzer ist allergisch gegen Meeresfrüchte",
		"chinese":  "用户对贝类过敏",
		"japanese": "ユーザーは甲殻類にアレルギーがあります",
	} {
		if got := cos(en, e.Encode(text)); got <= unrelated {
			t.Errorf("%s translation scored %.4f, no better than an unrelated English record at %.4f",
				lang, got, unrelated)
		}
	}
}

// Long text must be chunked rather than truncated, through the TEXT
// entry point — the one a caller actually uses.
//
// The assertion is a COMPARISON, not a threshold, and the first
// version of this test got that wrong. Two 1,900-token documents
// differing only in their final sentence score 0.99998 when chunked:
// the difference is real but diluted across four chunks, so any
// absolute threshold tight enough to prove the tail was read is also
// tight enough to fail on honest averaging.
//
// Truncation, meanwhile, gives EXACTLY 1.0 — bit-identical vectors,
// because the differing sentence is past the context limit and was
// never seen at all. That exactness is the signal, so the test asserts
// against it directly: chunked must differ, truncated must not.
func TestEncodeChunksRatherThanTruncates(t *testing.T) {
	e, err := Open(modelDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	tok, err := LoadTokenizer(filepath.Join(modelDir(t), "tokenizer.json"))
	if err != nil {
		t.Fatal(err)
	}

	var head string
	for range 150 {
		head += "the sourdough starter is fed on tuesdays. "
	}
	a := head + "and the cat is called Mabel."
	b := head + "and the boat is moored at Whitby."

	if n := len(tok.Encode(a)); n <= e.MaxSeq() {
		t.Fatalf("fixture is only %d tokens against a %d limit, so it never reaches the chunking path",
			n, e.MaxSeq())
	}

	// Truncated: the differing tail is past the limit, so the two
	// inputs reduce to the SAME ids and the vectors must be
	// bit-identical. Compared element-wise rather than by cosine —
	// cos(v, v) is not reliably exactly 1.0, because sqrt(n)*sqrt(n)
	// need not equal n in floating point, and an earlier version of
	// this test failed on precisely that.
	ta := e.model.Embed(tok.EncodeWithSpecials(a, e.MaxSeq()))
	tb := e.model.Embed(tok.EncodeWithSpecials(b, e.MaxSeq()))
	for i := range ta {
		if ta[i] != tb[i] {
			t.Fatalf("truncated vectors differ at %d — the fixture no longer exercises truncation", i)
		}
	}

	// Chunked: the tail IS read, so the vectors must differ. The
	// difference is small (~1e-5) because one sentence is diluted
	// across four chunks, which is why this is a comparison against
	// the truncated case rather than a threshold.
	ca, cb := e.Encode(a), e.Encode(b)
	same := true
	for i := range ca {
		if ca[i] != cb[i] {
			same = false
			break
		}
	}
	if same {
		t.Errorf("chunked vectors are identical; the tail past the context limit was not read")
	}
}
