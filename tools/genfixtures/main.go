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
	Text string `json:"text"`
	Note string `json:"note,omitempty"`
	// TokenIDs are the ids AS FED TO THE FORWARD PASS — wrapped with the
	// model's special tokens and truncated — not the raw tokenizer
	// output. That distinction is the whole point: it lets the forward
	// pass be gated on ids -> vector INDEPENDENTLY of whether our own
	// tokenizer is finished, so a numerics bug and a tokenizer bug can
	// never be mistaken for one another.
	TokenIDs []int32 `json:"token_ids"`
	// Vector is Embed(TokenIDs): pooled and L2-normalised.
	Vector []float32 `json:"vector"`
}

type Golden struct {
	Model     string    `json:"model"`
	HiddenDim int       `json:"hidden_dim"`
	Pooling   string    `json:"pooling"`
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
	maxSeqFlag := flag.Int("max-seq", 512, "sequence truncation length")
	out := flag.String("out", "", "directory to write golden.json into")
	flag.Parse()
	maxSeq := *maxSeqFlag
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

	g := Golden{Model: filepath.Base(*model), Pooling: "mean"}
	for _, c := range corpus {
		ids, err := tok.EncodeWithSpecials(c.text, maxSeq)
		if err != nil {
			fmt.Fprintf(os.Stderr, "tokenize %q: %v\n", c.text, err)
			os.Exit(1)
		}
		// Embed rather than Encode: this is the ids -> vector contract.
		vec := m.Embed(ids)
		if g.HiddenDim == 0 {
			g.HiddenDim = len(vec)
		}
		g.Fixtures = append(g.Fixtures, Fixture{
			Text: c.text, Note: c.note, TokenIDs: ids, Vector: vec,
		})
	}

	// Per MODEL. Fixtures are checkpoint-specific — the vectors are
	// this model's and nobody else's — so one shared file meant only
	// one model could ever have a committed gate. CI runs the small
	// multilingual checkpoint; a larger one can be verified locally
	// against its own set.
	outDir := filepath.Join(*out, fingerprint(*model))
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path := filepath.Join(outDir, "golden.json")
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

	if err := genTokens(tok, outDir); err != nil {
		fmt.Fprintf(os.Stderr, "token fixtures: %v\n", err)
		os.Exit(1)
	}
}

// fingerprint mirrors embedder.Model.Fingerprint, read straight from
// config.json so the generator needs no loaded model.
//
// Keyed on the CHECKPOINT rather than on its directory name: an earlier
// version used the folder, which made the fixtures' identity depend on
// what somebody called it.
func fingerprint(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "read config.json: %v\n", err)
		os.Exit(1)
	}
	var cfg struct {
		Hidden int `json:"hidden_size"`
		Layers int `json:"num_hidden_layers"`
		Vocab  int `json:"vocab_size"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "parse config.json: %v\n", err)
		os.Exit(1)
	}
	return fmt.Sprintf("d%d-l%d-v%d", cfg.Hidden, cfg.Layers, cfg.Vocab)
}
