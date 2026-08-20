package embedder

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// THE PARITY GATE.
//
// Every failure mode in a transformer forward pass is silent. A tanh
// GELU where the erf form was meant, an epsilon outside the sqrt, mean
// pooling where the checkpoint declares CLS, an attention scale of
// 1/sqrt(hidden), position row 0 where RoBERTa starts at 2 — none of
// them crash, none of them produce NaN, and all of them just make
// every vector permanently a little worse.
//
// So correctness here is not a judgement call. These vectors come from
// an independent implementation that is itself parity-gated against
// the reference (see doc.go for how to regenerate), and this test is the only
// thing standing between "it compiles and returns plausible floats"
// and "it is right".

type goldenFixture struct {
	Text     string    `json:"text"`
	Note     string    `json:"note,omitempty"`
	TokenIDs []int32   `json:"token_ids"`
	Vector   []float32 `json:"vector"`
}

type goldenFile struct {
	Model     string          `json:"model"`
	HiddenDim int             `json:"hidden_dim"`
	Pooling   string          `json:"pooling"`
	Fixtures  []goldenFixture `json:"fixtures"`
}

// fixtureDir is the fixture set for whichever checkpoint the tests
// were pointed at.
//
// Keyed on the model directory's name, because fixtures are
// checkpoint-specific: the vectors are that model's and nobody else's.
// Running the small model's gate against the base model would fail on
// every case and look like a broken forward pass.
func fixtureDir(t *testing.T) string {
	t.Helper()
	m, err := Load(modelDir(t))
	if err != nil {
		t.Fatalf("load model to identify its fixtures: %v", err)
	}
	defer func() { _ = m.Close() }()
	return filepath.Join("testdata", "golden", m.Fingerprint())
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureDir(t), "golden.json"))
	if err != nil {
		t.Fatalf("read golden fixtures: %v", err)
	}
	var g goldenFile
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("parse golden fixtures: %v", err)
	}
	if len(g.Fixtures) == 0 {
		t.Fatal("golden fixture set is empty — the gate would pass vacuously")
	}
	return g
}

// modelDir is where the checkpoint lives. Weights are ~1.1 GB, far too
// large to commit, so the parity test is opt-in via the environment
// and skips cleanly without it. The fixture file itself IS committed,
// so the shape and integrity checks below always run.
func modelDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("LOBSLAW_EMBEDDER_MODEL")
	if dir == "" {
		t.Skip("set LOBSLAW_EMBEDDER_MODEL to a HF snapshot directory to run the parity gate")
	}
	return dir
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func maxAbsDiff(a, b []float32) float64 {
	var worst float64
	for i := range a {
		if d := math.Abs(float64(a[i]) - float64(b[i])); d > worst {
			worst = d
		}
	}
	return worst
}

// TestGoldenParity is the whole point of the package.
//
// Tolerance rather than equality because float32 addition is not
// associative: a blocked matmul and a row-at-a-time matmul sum the
// same products in different orders and land a few ULP apart. That is
// expected and harmless. What is NOT harmless is a systematic
// difference, which any of the failure modes above produces — they
// move cosine well below this threshold, not marginally.
func TestGoldenParity(t *testing.T) {
	g := loadGolden(t)
	dir := modelDir(t)

	m, err := Load(dir)
	if err != nil {
		t.Fatalf("load model: %v", err)
	}
	if m.Dim() != g.HiddenDim {
		t.Fatalf("model dim = %d, fixtures were generated at %d — wrong checkpoint", m.Dim(), g.HiddenDim)
	}
	if string(m.Pooling()) != g.Pooling {
		t.Fatalf("pooling = %q, fixtures used %q", m.Pooling(), g.Pooling)
	}

	for _, f := range g.Fixtures {
		t.Run(f.Note, func(t *testing.T) {
			got := m.Embed(f.TokenIDs)
			if len(f.TokenIDs) == 0 {
				// The empty sequence has no tokens to pool. It must
				// return a zero vector rather than NaN — l2norm
				// refuses to divide by zero precisely for this.
				for _, v := range got {
					if math.IsNaN(float64(v)) {
						t.Fatal("empty input produced NaN")
					}
				}
				return
			}
			if len(got) != len(f.Vector) {
				t.Fatalf("dim = %d, want %d", len(got), len(f.Vector))
			}
			// MAX-ABS-DIFF, not cosine, and the thresholds are
			// measured rather than guessed.
			//
			// Cosine is almost useless as a gate here. Substituting
			// the tanh GELU approximation — a genuine, permanent
			// degradation — still scores 0.9999955. Any cosine
			// threshold loose enough not to be flaky is loose enough
			// to pass that.
			//
			// Per-element difference discriminates cleanly, measured
			// across this fixture set:
			//
			//	correct implementation   3.19e-07
			//	eps outside the sqrt     5.27e-06   (16x)
			//	tanh GELU                4.23e-04   (1300x)
			//
			// 1.5e-6 sits in that gap: ~5x headroom over honest
			// float32 reassociation, ~3.5x below the tightest real
			// defect. Both survived a 0.9999 cosine gate.
			const maxDiff = 1.5e-6
			if d := maxAbsDiff(got, f.Vector); d > maxDiff {
				t.Errorf("max abs diff from reference = %.3e, want <= %.1e (cosine %.10f)\n  text: %q",
					d, maxDiff, cosine(got, f.Vector), f.Text)
			}
			// Kept as a second axis: a catastrophic failure (wrong
			// pooling, transposed weights) shows up here immediately
			// and gives a far more legible message than a diff.
			if c := cosine(got, f.Vector); c < 0.99999 {
				t.Errorf("cosine against reference = %.8f, want >= 0.99999\n  text: %q", c, f.Text)
			}
		})
	}
}

// The fixture set must keep covering the cases that break naive
// implementations. Without this, a well-meaning tidy-up that deletes
// the awkward entries leaves a gate that passes everything.
func TestTheFixtureSetStillCoversTheHardCases(t *testing.T) {
	g := loadGolden(t)
	seen := map[string]bool{}
	for _, f := range g.Fixtures {
		seen[f.Note] = true
	}
	for _, want := range []string{
		"empty string — must not panic or divide by zero",
		"chinese: no whitespace between words",
		"arabic: right-to-left",
		"emoji — multi-byte outside the BMP",
		"the SAME word decomposed (NFD) — must normalise identically",
	} {
		if !seen[want] {
			t.Errorf("fixture %q has gone missing from the gate", want)
		}
	}
}

// Vectors are L2-normalised by construction, so any fixture whose norm
// is not 1 was generated wrongly and would make the parity threshold
// meaningless.
func TestGoldenVectorsAreNormalised(t *testing.T) {
	g := loadGolden(t)
	for _, f := range g.Fixtures {
		if len(f.Vector) == 0 {
			continue
		}
		var sum float64
		for _, v := range f.Vector {
			sum += float64(v) * float64(v)
		}
		if n := math.Sqrt(sum); math.Abs(n-1) > 1e-4 {
			t.Errorf("fixture %q has norm %.6f, want 1", f.Note, n)
		}
	}
}
