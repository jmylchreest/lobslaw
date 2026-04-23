package soul

import (
	"strings"
	"sync"

	"github.com/pemistahl/lingua-go"
)

// Detector is the narrow language-detection interface the agent
// uses. Narrow so tests can substitute a fake without pulling
// lingua-go into their import graph, and so the production
// implementation can lazily construct its classifier pool only
// when detection is actually enabled.
type Detector interface {
	// Detect returns an ISO 639-1 language code (e.g. "en", "de",
	// "zh") inferred from the sample. Empty string means "couldn't
	// confidently tell" — caller falls back to the configured
	// default language.
	Detect(sample string) string
}

// LinguaDetector wraps pemistahl/lingua-go. Construction is
// expensive (loads N-gram tables for every language we opt into),
// so callers build once + reuse. Zero value is intentionally
// unusable — use NewLinguaDetector.
type LinguaDetector struct {
	once     sync.Once
	detector lingua.LanguageDetector
	// Preload bounded by the languages operators actually want —
	// loading all 75+ takes a few hundred ms and a substantial
	// memory footprint. The default set covers the most common
	// ten; operators with a narrower or wider need configure via
	// NewLinguaDetectorWith.
	languages []lingua.Language
}

// DefaultLanguages is the lingua preload set when operators don't
// specify one. Covers the majority of realistic user-first-language
// distributions without paying for all 75. Narrower sets make
// detection faster + more accurate on short samples.
var DefaultLanguages = []lingua.Language{
	lingua.English, lingua.Spanish, lingua.French, lingua.German,
	lingua.Italian, lingua.Portuguese, lingua.Dutch,
	lingua.Russian, lingua.Chinese, lingua.Japanese,
}

// NewLinguaDetector builds a detector preloaded with
// DefaultLanguages. First call to Detect lazily builds the
// n-gram tables — this is the expensive step.
func NewLinguaDetector() *LinguaDetector {
	return NewLinguaDetectorWith(DefaultLanguages...)
}

// NewLinguaDetectorWith lets operators tune the preload set.
// Passing a single language means "always return that language
// above confidence floor, else empty" — useful for deployments
// where messages are overwhelmingly one language and detection
// is a sanity-check rather than primary routing.
func NewLinguaDetectorWith(langs ...lingua.Language) *LinguaDetector {
	if len(langs) == 0 {
		langs = DefaultLanguages
	}
	return &LinguaDetector{languages: langs}
}

// Detect returns a best-guess ISO 639-1 code or "" when the
// sample is too short / ambiguous. Thread-safe: the first call
// wins the once.Do race and constructs the detector; subsequent
// calls hit the cached one.
func (d *LinguaDetector) Detect(sample string) string {
	sample = strings.TrimSpace(sample)
	if sample == "" {
		return ""
	}
	d.once.Do(func() {
		d.detector = lingua.NewLanguageDetectorBuilder().
			FromLanguages(d.languages...).
			Build()
	})
	lang, ok := d.detector.DetectLanguageOf(sample)
	if !ok {
		return ""
	}
	// lingua returns the code upper-cased (e.g. "EN"); normalise to
	// lowercase to match the conventional BCP 47 / HTTP form.
	return strings.ToLower(lang.IsoCode639_1().String())
}

// NullDetector is the no-op detector used when operators disable
// language detection. Always returns "" so callers fall back to
// their configured default.
type NullDetector struct{}

// Detect always returns "" — callers fall back to the configured
// default language.
func (NullDetector) Detect(_ string) string { return "" }

// NewDetector picks the right concrete implementation based on
// the language section of a loaded SoulConfig. Returns NullDetector
// when detect=false so callers don't need to guard their Detect
// calls with a conditional.
func NewDetector(cfg Soul) Detector {
	if !cfg.Config.Language.Detect {
		return NullDetector{}
	}
	return NewLinguaDetector()
}
