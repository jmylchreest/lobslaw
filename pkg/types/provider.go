package types

// ProviderConfig describes one LLM provider endpoint. Providers are
// identified by Label in chains and resolver config.
type ProviderConfig struct {
	Label        string    `json:"label"`    // "fast", "smart", "local-llama"
	Endpoint     string    `json:"endpoint"` // OpenAI-compatible base URL
	Model        string    `json:"model"`
	APIKeyRef    string    `json:"api_key_ref,omitempty"` // env:VAR, kms:arn, file:/path
	Capabilities []string  `json:"capabilities,omitempty"`
	TrustTier    TrustTier `json:"trust_tier"`
	// Pricing overrides the built-in pricing table for this
	// provider. Zero values mean "use the built-in default".
	Pricing *ProviderPricing `json:"pricing,omitempty"`
}

// ProviderPricing is the $-per-1K-tokens table entry used for turn
// budget accounting. Zero fields fall back to the hardcoded defaults.
type ProviderPricing struct {
	InputUSDPer1K  float64 `json:"input_usd_per_1k,omitempty"`
	OutputUSDPer1K float64 `json:"output_usd_per_1k,omitempty"`
	CachedUSDPer1K float64 `json:"cached_usd_per_1k,omitempty"`
}

// ChainConfig is an ordered set of provider steps (primary +
// optional reviewers). Picked by the resolver when triggers match.
type ChainConfig struct {
	Label        string       `json:"label"`
	Steps        []ChainStep  `json:"steps"`
	Trigger      ChainTrigger `json:"trigger"`
	MinTrustTier TrustTier    `json:"min_trust_tier,omitempty"`
}

// ChainStep is one step in a chain. Role is advisory metadata
// ("primary", "reviewer", "synthesizer"). PromptTemplate, when
// present, wraps the previous step's output — e.g. "Review this
// response for accuracy: {{response}}".
type ChainStep struct {
	Provider       string `json:"provider"` // label ref to a ProviderConfig
	Role           string `json:"role"`
	PromptTemplate string `json:"prompt_template,omitempty"`
}

// ChainTrigger is a predicate over the turn's analysis that picks
// this chain. The resolver evaluates triggers in order; first match
// wins. Always=true makes the chain the default.
type ChainTrigger struct {
	MinComplexity int      `json:"min_complexity,omitempty"` // 1-10
	Domains       []string `json:"domains,omitempty"`        // "code", "creative", ...
	Always        bool     `json:"always,omitempty"`
}
