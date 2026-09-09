package types

import "time"

// SoulConfig is the YAML frontmatter of SOUL.md. The freeform
// markdown body is loaded alongside, separately.
type SoulConfig struct {
	SchemaVersion int       `yaml:"schema_version,omitempty" json:"schema_version,omitempty"`
	Verbosity     Verbosity `yaml:"verbosity,omitempty" json:"verbosity,omitempty"`
	Name          string    `yaml:"name" json:"name"`
	Scope         string    `yaml:"scope" json:"scope"`
	Culture       string    `yaml:"culture" json:"culture"`
	Nationality   string    `yaml:"nationality" json:"nationality"`

	Language Language `yaml:"language" json:"language"`

	PersonaDescription string `yaml:"persona_description" json:"persona_description"`

	EmotiveStyle EmotiveStyle `yaml:"emotive_style" json:"emotive_style"`
	Adjustments  Adjustments  `yaml:"adjustments" json:"adjustments"`

	MinTrustTier TrustTier      `yaml:"min_trust_tier,omitempty" json:"min_trust_tier,omitempty"`
	Feedback     FeedbackConfig `yaml:"feedback" json:"feedback"`

	// Fragments are short anecdotal facts the agent has been asked
	// to remember about itself or the user ("user supports
	// Liverpool", "prefers tea over coffee"). Agent-writable via the
	// soul_fragment_* builtins; capped + sanitised on write to
	// prevent prompt-injection through self-modified prose. Rendered
	// into the system prompt as escaped bullets inside a clearly
	// delimited block — never as free-form prose.
	Fragments []string `yaml:"fragments,omitempty" json:"fragments,omitempty"`
}

// Language controls which language the agent replies in. When Detect
// is set, the incoming message's language wins over Default.
type Language struct {
	Default        string `yaml:"default" json:"default"`
	Detect         bool   `yaml:"detect" json:"detect"`
	SpellingLocale string `yaml:"spelling_locale,omitempty" json:"spelling_locale,omitempty"`
}

// SoulSchemaVersion opts into strict decoding and explicit-zero style scores.
const (
	SoulSchemaVersion int = 1
	SoulNeutralScore  int = 5
)

// Verbosity sets the default amount of explanation in replies.
type Verbosity string

// Supported verbosity levels are independent of the directness style dial.
const (
	VerbosityConcise  Verbosity = "concise"
	VerbosityBalanced Verbosity = "balanced"
	VerbosityDetailed Verbosity = "detailed"
)

// EmotiveStyle scores the soul on numeric dimensions (0-10) plus
// emoji_usage as "minimal" | "moderate" | "generous". Dynamic
// adjustment mutates these within ±3 of the baseline.
type EmotiveStyle struct {
	EmojiUsage string `yaml:"emoji_usage" json:"emoji_usage"`
	Excitement int    `yaml:"excitement" json:"excitement"`
	Formality  int    `yaml:"formality" json:"formality"`
	Directness int    `yaml:"directness" json:"directness"`
	Sarcasm    int    `yaml:"sarcasm" json:"sarcasm"`
	Humor      int    `yaml:"humor" json:"humor"`
}

// Adjustments bounds the dynamic tuning of EmotiveStyle.
// FeedbackCoefficient scales how far a single piece of feedback can
// move a dimension; CooldownPeriod is the minimum gap between two
// adjustments of the same dimension.
type Adjustments struct {
	FeedbackCoefficient float64       `yaml:"feedback_coefficient" json:"feedback_coefficient"`
	CooldownPeriod      time.Duration `yaml:"cooldown_period" json:"cooldown_period"`
}

// FeedbackConfig selects how user feedback is classified before it
// feeds into soul adjustment. Classifier is "llm" (fast-tier
// provider call) or "regex" (pattern dictionary); the default is
// "llm" with a regex fallback.
type FeedbackConfig struct {
	Classifier string `yaml:"classifier" json:"classifier"`
}
