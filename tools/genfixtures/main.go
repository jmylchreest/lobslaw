// Command genfixtures produces the parity gate for lobslaw's own
// XLM-RoBERTa encoder.
//
// THIS IS A SCAFFOLD, NOT A DEPENDENCY. It lives in its own module so
// that aikit never appears in lobslaw's go.mod and never ships in the
// binary. We are borrowing aikit's correctness — which is parity-gated
// against the reference implementation — to build a fixture set we own
// outright. Once internal/embedder reproduces these vectors, aikit has
// done its job and this directory can be deleted without touching the
// shipped code.
//
// The alternative was writing ~900 lines of numerics with no reference
// to check against. Every failure mode in a transformer forward pass is
// SILENT: a tanh GELU where the exact erf form was meant, an epsilon
// outside the sqrt instead of inside, mean pooling where the model
// declares CLS, a 1/sqrt(hidden) attention scale instead of
// 1/sqrt(head_dim). None of those crash. They all just make every
// vector slightly worse, for ever, with no symptom but worse recall.
//
// Usage:
//
//	go run . -model ~/models/multilingual-e5-base -out ../../internal/embedder/testdata/golden
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/townsendmerino/aikit/embed"
	"github.com/townsendmerino/aikit/encoder"
)

// Fixture is one (text → tokens → vector) triple.
//
// Token ids are recorded ALONGSIDE the vector, not instead of it,
// because they fail differently. A tokenizer that is subtly wrong still
// produces a plausible vector, so a vector-only fixture would let a
// broken tokenizer pass whenever the embedding happened to land close.
// Ids are exact integers: they match or they do not.
type Fixture struct {
	Text     string    `json:"text"`
	Note     string    `json:"note,omitempty"`
	TokenIDs []int32   `json:"token_ids"`
	Vector   []float32 `json:"vector"`
}

type Golden struct {
	Model     string    `json:"model"`
	HiddenDim int       `json:"hidden_dim"`
	Fixtures  []Fixture `json:"fixtures"`
}

// corpus deliberately spans the cases that break naive implementations
// rather than the cases that work.
var corpus = []struct{ text, note string }{
	{"the user is based in Yorkshire", "plain ascii"},
	{"prefers TOML over YAML for configuration", "acronyms + casing"},
	{"", "empty string — must not panic or divide by zero"},
	{"a", "single character, shorter than any subword"},
	{"l'utilisateur est allergique aux fruits de mer", "french: apostrophe + accents"},
	{"der Benutzer ist allergisch gegen Meeresfrüchte", "german: umlaut"},
	{"用户对贝类过敏", "chinese: no whitespace between words"},
	{"ユーザーは甲殻類にアレルギーがあります", "japanese: mixed kana + kanji"},
	{"пользователь живёт в Йоркшире", "cyrillic"},
	{"المستخدم يعيش في يوركشاير", "arabic: right-to-left"},
	{"🍤 shellfish allergy 🚨", "emoji — multi-byte outside the BMP"},
	{"café", "precomposed vs decomposed accent (NFC)"},
	{"café", "the SAME word decomposed (NFD) — must normalise identically"},
	{"   leading and trailing   ", "whitespace handling"},
	{"supercalifragilisticexpialidocious", "long OOV word — exercises subword splitting"},
	{"Ruth is getting married in June and the venue is in Harrogate", "longer sentence"},
}

func main() {
	model := flag.String("model", "", "path to an HF snapshot directory")
	out := flag.String("out", "", "directory to write golden.json into")
	flag.Parse()
	if *model == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "both -model and -out are required")
		os.Exit(2)
	}

	m, err := encoder.LoadBERT(*model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load %s: %v\n", *model, err)
		os.Exit(1)
	}
	defer func() { _ = m.Close() }()

	// The tokenizer is loaded SEPARATELY from the same snapshot so the
	// ids can be recorded. BERT.Encode only returns the vector, and a
	// vector-only fixture would let a subtly wrong tokenizer pass
	// whenever its embedding happened to land close enough.
	tok, err := embed.LoadTokenizer(filepath.Join(*model, "tokenizer.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "load tokenizer: %v\n", err)
		os.Exit(1)
	}

	g := Golden{Model: filepath.Base(*model)}
	for _, c := range corpus {
		vec, err := m.Encode(c.text)
		if err != nil {
			// Recorded as a fixture with a nil vector would be a lie —
			// better to fail loudly than to bake an error into the gate.
			fmt.Fprintf(os.Stderr, "encode %q: %v\n", c.text, err)
			os.Exit(1)
		}
		if g.HiddenDim == 0 {
			g.HiddenDim = len(vec)
		}
		g.Fixtures = append(g.Fixtures, Fixture{
			Text: c.text, Note: c.note, TokenIDs: tok.Encode(c.text), Vector: vec,
		})
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(*out, "golden.json")
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s: %d fixtures, hidden_dim=%d\n", path, len(g.Fixtures), g.HiddenDim)
}
