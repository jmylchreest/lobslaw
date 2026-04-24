package types

import "time"

// SoulConfig is the YAML frontmatter of SOUL.md. The freeform
// markdown body is loaded alongside, separately.
type SoulConfig struct {
	Name        string `yaml:"name" json:"name"`
	Scope       string `yaml:"scope" json:"scope"`
	Culture     string `yaml:"culture" json:"culture"`
	Nationality string `yaml:"nationality" json:"nationality"`

	// Project + Repository + Workspace let the agent answer
	// "look at <project> on GitHub" by recognising itself as the
	// project being referenced. Without these the bot will search
	// the web for similarly-named projects and confidently report
	// findings about a stranger codebase. Operator sets these
	// when the deployment IS a known software project; personal-
	// assistant deployments without code can leave them empty.
	Project    string `yaml:"project,omitempty" json:"project,omitempty"`
	Repository string `yaml:"repository,omitempty" json:"repository,omitempty"`
	Workspace  string `yaml:"workspace,omitempty" json:"workspace,omitempty"`

	Language Language `yaml:"language" json:"language"`

	PersonaDescription string `yaml:"persona_description" json:"persona_description"`

	EmotiveStyle EmotiveStyle `yaml:"emotive_style" json:"emotive_style"`
	Adjustments  Adjustments  `yaml:"adjustments" json:"adjustments"`

	MinTrustTier TrustTier      `yaml:"min_trust_tier,omitempty" json:"min_trust_tier,omitempty"`
	Feedback     FeedbackConfig `yaml:"feedback" json:"feedback"`
}

type Language struct {
	Default string `yaml:"default" json:"default"`
	Detect  bool   `yaml:"detect" json:"detect"`
}

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

type Adjustments struct {
	FeedbackCoefficient float64       `yaml:"feedback_coefficient" json:"feedback_coefficient"`
	CooldownPeriod      time.Duration `yaml:"cooldown_period" json:"cooldown_period"`
}

// FeedbackConfig.Classifier is "llm" (fast-tier provider call) or
// "regex" (pattern dictionary). Default "llm" with regex fallback.
type FeedbackConfig struct {
	Classifier string `yaml:"classifier" json:"classifier"`
}
