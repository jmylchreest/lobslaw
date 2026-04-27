package config

import (
	"path/filepath"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Config is the top-level lobslaw configuration. Each subsystem
// validates its own slice — this layer only parses and resolves
// secret references.
type Config struct {
	Memory        MemoryConfig        `koanf:"memory"`
	Storage       StorageConfig       `koanf:"storage"`
	Policy        PolicyConfig        `koanf:"policy"`
	Compute       ComputeConfig       `koanf:"compute"`
	Hooks         HooksConfig         `koanf:"hooks"`
	Gateway       GatewayConfig       `koanf:"gateway"`
	Discovery     DiscoveryConfig     `koanf:"discovery"`
	Cluster       ClusterConfig       `koanf:"cluster"`
	Soul          SoulLoaderConfig    `koanf:"soul"`
	Scheduler     SchedulerConfig     `koanf:"scheduler"`
	Auth          AuthConfig          `koanf:"auth"`
	Sandbox       SandboxConfig       `koanf:"sandbox"`
	Audit         AuditConfig         `koanf:"audit"`
	Skills        SkillsConfig        `koanf:"skills"`
	Observability ObservabilityConfig `koanf:"observability"`
	Logging       LoggingConfig       `koanf:"logging"`
	MCP           MCPConfig           `koanf:"mcp"`
	ConfigOpts    ConfigOpts          `koanf:"config"`

	// resolvedPath is the filesystem path Load resolved via
	// findConfigPath. Empty when no config.toml was found (env-only
	// mode). Not populated from any TOML source (koanf:"-") — filled
	// in at Load time.
	resolvedPath string `koanf:"-"`
}

// Path returns the filesystem path of the config file Load resolved.
// Empty string when Load ran in env-only mode (no config.toml found).
func (c *Config) Path() string { return c.resolvedPath }

// Dir returns the directory containing the resolved config file.
// Downstream code uses this to derive sibling paths (e.g. policy.d/)
// without introducing a parallel env-var / discovery chain.
// Empty string when Path is empty.
func (c *Config) Dir() string {
	if c.resolvedPath == "" {
		return ""
	}
	return filepath.Dir(c.resolvedPath)
}

type MemoryConfig struct {
	Enabled    bool             `koanf:"enabled"`
	RaftPort   int              `koanf:"raft_port"`
	Encryption EncryptionConfig `koanf:"encryption"`
	Snapshot   SnapshotConfig   `koanf:"snapshot"`
	Dream      DreamConfig      `koanf:"dream"`
	Session    SessionConfig    `koanf:"session"`
}

// SessionConfig governs the auto-seeded session retention pruner.
// Distinct from DreamConfig: dream is consolidation (turns many
// records into a summary); session prune is hard-deletion of
// transient retention=session records past their TTL. Default ON
// at hourly cadence with a 24h max-age.
type SessionConfig struct {
	// Enabled controls whether lobslaw auto-seeds the recurring
	// session prune task. *bool so unset (default ON) is
	// distinguishable from explicit disable.
	Enabled *bool `koanf:"enabled"`
	// Schedule is the cron expression for the auto-seeded prune
	// task. Empty → "@hourly". Use a slower cadence on chatty
	// deployments; the prune itself is cheap (linear bucket scan +
	// per-stale-record raft.Apply).
	Schedule string `koanf:"schedule"`
	// MaxAge is the TTL beyond which a retention=session record
	// becomes a prune candidate. Empty/zero → 24h.
	MaxAge time.Duration `koanf:"max_age"`
}

type EncryptionConfig struct {
	KeyRef string `koanf:"key_ref"`
}

type SnapshotConfig struct {
	Target    string        `koanf:"target"`
	Cadence   time.Duration `koanf:"cadence"`
	Retention string        `koanf:"retention"`
}

type DreamConfig struct {
	// Enabled controls whether lobslaw auto-seeds a recurring Dream
	// pass at boot. *bool so we can distinguish "operator left it
	// unset" (default ON) from "operator explicitly turned it off"
	// (default OFF semantics impossible to recover otherwise).
	Enabled *bool `koanf:"enabled"`
	// Schedule is the cron expression for the auto-seeded Dream
	// task. Empty → "0 2 * * *" (02:00 daily). Operators can also
	// declare their own [[scheduler.tasks]] with handler="memory:dream"
	// for non-recurring or differently-scoped passes; the auto-seed
	// uses the well-known ID "lobslaw-builtin-dream" so it doesn't
	// collide with operator-defined entries.
	Schedule string `koanf:"schedule"`
}

type StorageConfig struct {
	Enabled bool                 `koanf:"enabled"`
	Mounts  []StorageMountConfig `koanf:"mounts"`
}

type StorageMountConfig struct {
	Label            string            `koanf:"label"`
	Type             string            `koanf:"type"`
	Path             string            `koanf:"path,omitempty"`
	// Writable controls whether the agent can write/edit files
	// inside this mount via builtin fs tools. Default false — the
	// operator must opt in explicitly, because a default-writable
	// mount could corrupt internal state (snapshots, bbolt files)
	// if its path overlaps with cluster-private directories.
	Writable bool `koanf:"writable,omitempty"`
	// Excludes is a list of glob patterns (e.g. ".git/**", "*.key",
	// "node_modules/**") hidden from list/glob/grep/read inside
	// this mount. Hardcoded internal excludes (.snapshot, state.db,
	// *.pem) always apply on top of these.
	Excludes []string `koanf:"excludes,omitempty"`
	Bucket           string            `koanf:"bucket,omitempty"`
	Endpoint         string            `koanf:"endpoint,omitempty"`
	Account          string            `koanf:"account,omitempty"`
	AccessKeyRef     string            `koanf:"access_key_ref,omitempty"`
	SecretKeyRef     string            `koanf:"secret_key_ref,omitempty"`
	Env              string            `koanf:"env,omitempty"` // multi-line KEY=VAL pairs
	Crypt            bool              `koanf:"crypt,omitempty"`
	CryptPasswordRef string            `koanf:"crypt_password_ref,omitempty"`
	CryptSaltRef     string            `koanf:"crypt_salt_ref,omitempty"`
	ExtraOpts        map[string]string `koanf:"extra_opts,omitempty"`
}

type PolicyConfig struct {
	Enabled bool `koanf:"enabled"`
	// Rules are operator-declared [[policy.rules]] entries seeded
	// at boot via raft. Each rule mirrors lobslawv1.PolicyRule
	// fields. Subjects MUST be "kind:value" (scope:owner,
	// user:alice, role:admin) or "*" — bare strings like "owner"
	// are treated as malformed (fail-closed) by the engine.
	// Higher Priority wins. Default-deny seeds for builtins land
	// at priority=10; operator allow rules typically use 20+.
	Rules []PolicyRuleConfig `koanf:"rules,omitempty"`
}

type PolicyRuleConfig struct {
	ID       string `koanf:"id"`
	Subject  string `koanf:"subject"`
	Action   string `koanf:"action"`
	Resource string `koanf:"resource"`
	Effect   string `koanf:"effect"`              // "allow" | "deny"
	Priority int32  `koanf:"priority,omitempty"`  // higher wins
}

type ComputeConfig struct {
	Enabled             bool             `koanf:"enabled"`
	Providers           []ProviderConfig `koanf:"providers"`
	Chains              []ChainConfig    `koanf:"chains"`
	DefaultChain        string           `koanf:"default_chain"`
	ComplexityEstimator string           `koanf:"complexity_estimator"`
	Budgets             BudgetsConfig    `koanf:"budgets"`  // deprecated; use Limits
	Limits              LimitsConfig     `koanf:"limits,omitempty"`
	Plugins             []PluginConfig   `koanf:"plugins"`
	WebSearch           WebSearchConfig  `koanf:"web_search,omitempty"`
	Vision              VisionConfig     `koanf:"vision,omitempty"`
	Audio               AudioConfig      `koanf:"audio,omitempty"`
	PDF                 PDFConfig        `koanf:"pdf,omitempty"`
	Embeddings          EmbeddingsConfig `koanf:"embeddings,omitempty"`
	// Roles maps named functional roles (main, preflight,
	// reranker, summariser, etc.) to provider labels. Internal
	// code asks the resolver for a role by name; the resolver
	// dereferences to the provider. Empty → first provider fills
	// every role (today's behaviour).
	Roles RolesConfig `koanf:"roles,omitempty"`
}

// RolesConfig names the provider labels for each agent role.
// Keeping this as named fields rather than a map makes misspelled
// roles a compile-time error and lets the TOML reader validate
// shape. Add new roles here as we need them.
type RolesConfig struct {
	// Main is the provider for the user-facing agent turn. Empty
	// → first provider (back-compat).
	Main string `koanf:"main,omitempty"`

	// Preflight is the cheap model used for context-engine
	// classification and prompt tailoring. Empty → Main.
	Preflight string `koanf:"preflight,omitempty"`

	// Reranker is the model used for memory rerank (two-stage
	// RAG). Empty → Preflight.
	Reranker string `koanf:"reranker,omitempty"`

	// Summariser is the model used for dream consolidation /
	// episodic summarisation. Empty → Main.
	Summariser string `koanf:"summariser,omitempty"`
}

// WebSearchConfig enables the Exa-backed web_search builtin. When
// APIKeyRef is empty, the builtin is not registered and the model
// sees no web_search tool. MCP-sourced web_search registrations
// (future) override the builtin by virtue of later-registration
// wins in the tool registry.
type WebSearchConfig struct {
	APIKeyRef string `koanf:"api_key_ref,omitempty"`
	Endpoint  string `koanf:"endpoint,omitempty"`
}

// VisionConfig enables the read_image builtin — a tool the agent
// calls to get a textual description of an image at a local path.
// Required when the main LLM is text-only (e.g. MiniMax-M2):
// Telegram downloads the attachment to /workspace/incoming/<turn>/,
// the prompt-decoration tells the agent the path, and read_image
// is the tool the agent invokes to actually inspect it.
//
// The builtin POSTs to an OpenAI-compatible /chat/completions
// endpoint with a multimodal user message ({type:"image_url",
// image_url:{url:"data:image/jpeg;base64,..."}} + optional text
// prompt). Any vision-capable provider works: MiniMax's
// abab6.5s-chat / MiniMax-VL-01, Google Gemini Flash, OpenAI
// gpt-4o-mini, Anthropic claude-3-5-haiku, etc.
//
// ModalityOverride pins a modality builtin to one specific provider
// label, bypassing capability auto-discovery. Empty Provider →
// auto-discovery picks from [[compute.providers]] entries tagged
// with the matching capability (highest Priority wins). Operators
// only need this when they have multiple capability-matching
// providers and want a non-priority pick for a specific modality.
type ModalityOverride struct {
	Provider string `koanf:"provider,omitempty"`
}

// VisionConfig / AudioConfig / PDFConfig are thin override shells.
// 99% of operators leave them empty — declaring a provider with
// capabilities = ["vision"] (etc) is enough.
type VisionConfig = ModalityOverride
type AudioConfig = ModalityOverride
type PDFConfig = ModalityOverride

// EmbeddingsConfig points at an embeddings endpoint. Empty
// Endpoint → no embedder wired, memory_search falls back to
// substring match and auto-ingest skips vector-record writes.
// Dims MUST match the model's actual output dimension; mismatches
// surface as runtime errors on every call.
//
// Format picks the request/response protocol:
//
//	"openai"  — {input, model} → {data: [{embedding: []}]}.
//	            Used by OpenAI, OpenRouter, z.ai, most providers.
//	            Default when Format is empty.
//	"minimax" — {texts: [], model, type} → {vectors: []}.
//	            MiniMax's native protocol via api.minimax.io/v1.
//
// Auto-detect is deliberately NOT supported: a probe-on-first-
// call pattern wastes tokens and fails silently when credentials
// are wrong. Operators declare the format explicitly.
type EmbeddingsConfig struct {
	Endpoint  string `koanf:"endpoint,omitempty"`
	Model     string `koanf:"model,omitempty"`
	APIKeyRef string `koanf:"api_key_ref,omitempty"`
	Dims      int    `koanf:"dims,omitempty"`
	Format    string `koanf:"format,omitempty"`
}

// ProviderConfig describes one LLM endpoint. Format is the wire
// protocol — "openai" (default) covers OpenAI, OpenRouter, MiniMax,
// z.ai and any vendor that speaks /chat/completions; "anthropic"
// covers Claude's native /v1/messages; "gemini" covers Google AI
// Studio's generateContent. Modality builtins (read_image,
// read_audio, read_pdf, embeddings) discover providers via the
// Capabilities tags + Priority — operators don't wire each builtin
// separately; they tag a provider and the right builtin picks it up.
//
// Capability tokens consumed today:
//
//	"chat", "function-calling" — main agent loop / chains
//	"vision"                   — read_image
//	"audio-transcription"      — read_audio (Whisper multipart)
//	"audio-multimodal"         — read_audio (chat-completions input_audio)
//	"pdf"                      — read_pdf (chat-completions file part)
//	"embeddings"               — vector embedding endpoint
//
// Higher Priority wins ties; declaration order breaks Priority ties.
type ProviderConfig struct {
	Label        string                `koanf:"label"`
	Endpoint     string                `koanf:"endpoint"`
	Model        string                `koanf:"model"`
	Format       string                `koanf:"format,omitempty"`
	Priority     int                   `koanf:"priority,omitempty"`
	// AutoCapabilities turns on models.dev capability discovery for
	// this provider entry. At node boot the catalog is fetched (24h
	// disk cache), the configured model is looked up, and the
	// discovered modalities are MERGED with declared capabilities.
	// Declared capabilities always win on conflict. Off by default.
	AutoCapabilities bool                  `koanf:"auto_capabilities,omitempty"`
	APIKeyRef    string                `koanf:"api_key_ref,omitempty"`
	Capabilities []string              `koanf:"capabilities,omitempty"`
	TrustTier    types.TrustTier       `koanf:"trust_tier"`
	Pricing      types.ProviderPricing `koanf:"pricing,omitempty"`

	// Backup is the label of the provider to fall back to when this
	// one fails with a transient hard error (5xx, rate-limit, network
	// refusal, timeout). Empty → end of chain, error surfaces to the
	// caller. Chains are walked same-turn so the user sees the reply
	// from whichever provider succeeds, transparently. Cycles are
	// rejected at config load.
	Backup string `koanf:"backup,omitempty"`
	// ServerTools are provider-side tools (e.g. OpenRouter's
	// openrouter:web_search) merged into every request's tools
	// array. Transparent to the Executor — the provider handles
	// them server-side and returns synthesised results. Use for
	// capabilities we don't want to implement ourselves.
	ServerTools []ServerToolSpec `koanf:"server_tools,omitempty"`
}

// ServerToolSpec is one provider-side tool. Parameters is a
// freeform JSON object the provider interprets — we don't validate
// beyond "well-formed JSON". Example for OpenRouter web search:
//
//	{type = "openrouter:web_search", parameters = {max_results = 5}}
type ServerToolSpec struct {
	Type       string         `koanf:"type"`
	Parameters map[string]any `koanf:"parameters,omitempty"`
}

type ChainConfig struct {
	Label        string             `koanf:"label"`
	Steps        []ChainStepConfig  `koanf:"steps"`
	Trigger      ChainTriggerConfig `koanf:"trigger"`
	MinTrustTier types.TrustTier    `koanf:"min_trust_tier,omitempty"`
}

type ChainStepConfig struct {
	Provider       string `koanf:"provider"`
	Role           string `koanf:"role"`
	PromptTemplate string `koanf:"prompt_template,omitempty"`
}

type ChainTriggerConfig struct {
	MinComplexity int      `koanf:"min_complexity,omitempty"`
	Domains       []string `koanf:"domains,omitempty"`
	Always        bool     `koanf:"always,omitempty"`
}

// BudgetsConfig is DEPRECATED — retained so existing TOML configs
// still parse without error, but the spend/egress fields are no-ops
// per lobslaw-per-turn-budgets (superseded). MaxToolCallsPerTurn is
// consumed by compute.FromConfig as a bridge to LimitsConfig during
// the deprecation window; new configs should put it under
// [compute.limits].
type BudgetsConfig struct {
	MaxToolCallsPerTurn   int     `koanf:"max_tool_calls_per_turn"`
	MaxSpendUSDPerTurn    float64 `koanf:"max_spend_usd_per_turn,omitempty"`   // deprecated: no-op
	MaxEgressBytesPerTurn int64   `koanf:"max_egress_bytes_per_turn,omitempty"` // deprecated: no-op
}

// MCPConfig describes top-level Model Context Protocol server
// declarations. Each server is a subprocess (typically via stdio)
// exposing a set of tools that appear alongside the built-in tools
// in the LLM's function list. Plugins can also declare servers
// via .mcp.json; both sources compose at boot.
type MCPConfig struct {
	// Servers maps a logical name (used as the tool namespace
	// prefix, e.g. "gmail" → tools appear as gmail.search) to the
	// subprocess specification.
	Servers map[string]MCPServerConfig `koanf:"servers"`
}

// MCPServerConfig is one server's subprocess specification.
// Command + Args compose the argv; Env pairs are plaintext;
// SecretEnv names env vars whose values resolve via secret refs
// (env:/file:/kms:) the same way every other lobslaw secret does.
type MCPServerConfig struct {
	Command   string            `koanf:"command"`
	Args      []string          `koanf:"args,omitempty"`
	Env       map[string]string `koanf:"env,omitempty"`
	SecretEnv map[string]string `koanf:"secret_env,omitempty"`
	Disabled  bool              `koanf:"disabled,omitempty"`

	// Install runs once before the server is spawned. Idempotent by
	// design — `uv tool install` / `bun install` no-op when the
	// requested version is already cached. Pinning the version here
	// (e.g. `["uv","tool","install","minimax-mcp==1.27.0"]`) is the
	// supply-chain boundary: lobslaw won't promote an arbitrary new
	// release without an operator config change. Failure is fatal
	// for that server (it doesn't spawn) but doesn't block boot.
	// Empty → spawn directly without installing (assume the binary
	// is already on PATH).
	Install []string `koanf:"install,omitempty"`
}

// LimitsConfig holds non-cost safety valves. These are about
// preventing runaway loops and pathological behaviour, not about
// rationing spend (which lobslaw doesn't gate on).
type LimitsConfig struct {
	// MaxToolCallsPerTurn caps how many tool invocations one turn
	// can chain before the agent forces a summary reply. Default 30
	// (applied at consumer time when zero). Protects against a
	// stuck LLM calling the same failing tool indefinitely.
	MaxToolCallsPerTurn int `koanf:"max_tool_calls_per_turn"`
}

type PluginConfig struct {
	Name              string `koanf:"name"`
	Source            string `koanf:"source"`
	AutoInstallBinary bool   `koanf:"auto_install_binary,omitempty"`
	Enabled           bool   `koanf:"enabled"`
}

// HooksConfig is keyed by event name (PreToolUse, PostToolUse, …).
// Each event may have multiple subprocess hooks.
type HooksConfig map[string][]types.HookConfig

type GatewayConfig struct {
	Enabled             bool                   `koanf:"enabled"`
	GRPCPort            int                    `koanf:"grpc_port"`
	HTTPPort            int                    `koanf:"http_port"`
	Channels            []GatewayChannelConfig `koanf:"channels"`
	ConfirmationTimeout time.Duration          `koanf:"confirmation_timeout"`
	UnknownUserScope    string                 `koanf:"unknown_user_scope"`

	// Responsiveness timers. Zero on any = disabled. Operators can
	// tune per deployment; sensible defaults land in Load().
	TypingInterval time.Duration `koanf:"typing_interval"` // refresh typing indicator (Telegram clears at ~5s)
	InterimTimeout time.Duration `koanf:"interim_timeout"` // send "still working" message after this (chatty SOUL only)
	HardTimeout    time.Duration `koanf:"hard_timeout"`    // cancel turn + force summary reply after this
}

type GatewayChannelConfig struct {
	Type string `koanf:"type"`
	// Mode picks "webhook" (default) or "poll" for telegram. Poll
	// mode needs no inbound network — right default for personal
	// deployments behind NAT. secret_token_ref is only required in
	// webhook mode.
	Mode           string `koanf:"mode,omitempty"`
	BotTokenRef    string `koanf:"bot_token_ref,omitempty"`
	SecretTokenRef string `koanf:"secret_token_ref,omitempty"`
	TLSCert        string `koanf:"tls_cert,omitempty"`
	TLSKey         string `koanf:"tls_key,omitempty"`
	// UserScopes maps channel-specific user IDs (Telegram user_id
	// as a string because TOML doesn't allow int keys) to lobslaw
	// security scopes. An unmapped user falls through to the
	// gateway's unknown_user_scope. For a personal bot, listing
	// your own user_id with scope="owner" locks everyone else out.
	UserScopes map[string]string `koanf:"user_scopes,omitempty"`

	// Webhook channel fields. Only consulted when Type == "webhook".
	// WebhookPath is the URL path mounted under the gateway HTTP
	// server (default "/webhook/<Name>"). SharedSecretRef auths
	// inbound requests via Authorization: Bearer <secret>. Scope
	// applied to dispatched turns; operator controls what the
	// inbound caller can do.
	Name             string `koanf:"name,omitempty"`
	WebhookPath      string `koanf:"webhook_path,omitempty"`
	SharedSecretRef  string `koanf:"shared_secret_ref,omitempty"`
	Scope            string `koanf:"scope,omitempty"`
}

type DiscoveryConfig struct {
	SeedNodes          []string      `koanf:"seed_nodes"`
	Broadcast          bool          `koanf:"broadcast"`
	BroadcastInterface string        `koanf:"broadcast_interface"`
	BroadcastPort      int           `koanf:"broadcast_port"`     // default 7445
	BroadcastAddress   string        `koanf:"broadcast_address"`  // default "255.255.255.255"
	BroadcastInterval  time.Duration `koanf:"broadcast_interval"` // default 30s
}

type ClusterConfig struct {
	// ListenAddr is host:port for the cluster-internal gRPC listener.
	// All cluster services (NodeService, MemoryService, PolicyService,
	// RaftTransport, etc.) bind here under mTLS.
	ListenAddr string `koanf:"listen_addr"`

	// AdvertiseAddr is what peers dial to reach this node. Empty means
	// derive from ListenAddr. k8s deployments set this to the pod IP or
	// stable service DNS; docker-compose typically leaves it empty.
	AdvertiseAddr string `koanf:"advertise_addr"`

	// DataDir is where state.db + raft.db + snapshots/ live for
	// memory/policy-enabled nodes.
	DataDir string `koanf:"data_dir"`

	// Bootstrap (default true) lets a node form a brand-new cluster
	// when it cannot join an existing one. On startup the node first
	// tries to join via [discovery] seed_nodes; if every seed fails
	// (or there are no seeds) within BootstrapTimeout, the node calls
	// raft.BootstrapCluster as the sole voter. Set to false on
	// joiners that must never form a fresh cluster on their own —
	// they fail-fast instead, which is the right policy for
	// production multi-node deployments where split-brain is worse
	// than refusing to start.
	Bootstrap *bool `koanf:"bootstrap"`

	// BootstrapTimeout caps how long the node spends trying to join
	// an existing cluster before falling back to solo-bootstrap (or
	// failing, if Bootstrap=false). Zero → 30s default.
	BootstrapTimeout time.Duration `koanf:"bootstrap_timeout"`

	MTLS MTLSConfig `koanf:"mtls"`
}

// MTLSConfig deliberately does NOT carry the CA private key path —
// that field exists only on the `cluster sign-node` subcommand. The
// main lobslaw binary cannot read the CA key.
type MTLSConfig struct {
	CACert   string `koanf:"ca_cert"`
	NodeCert string `koanf:"node_cert"`
	NodeKey  string `koanf:"node_key"`
}

type SoulLoaderConfig struct {
	Path  string `koanf:"path"`
	Scope string `koanf:"scope"`
}

type SchedulerConfig struct {
	Enabled      bool                  `koanf:"enabled"`
	TickInterval time.Duration         `koanf:"tick_interval"`
	ClaimLease   time.Duration         `koanf:"claim_lease"`
	Tasks        []SchedulerTaskConfig `koanf:"tasks"`
}

type SchedulerTaskConfig struct {
	Name     string `koanf:"name"`
	Schedule string `koanf:"schedule"`
	Handler  string `koanf:"handler"`
	Enabled  bool   `koanf:"enabled"`
}

type AuthConfig struct {
	Issuer       string `koanf:"issuer"`
	JWKSURL      string `koanf:"jwks_url"`
	JWTSecretRef string `koanf:"jwt_secret_ref,omitempty"`
	AllowHS256   bool   `koanf:"allow_hs256"`

	// RequireAuth makes missing or invalid Authorization tokens a
	// hard 401 on channels that honour it (REST today). Leave false
	// for localhost / reverse-proxy-terminated deployments where
	// auth is checked upstream; set true for anything reachable from
	// the public internet. Unset-and-validator-configured is
	// intentional: "accept valid tokens, fall back to default scope
	// for anonymous" is the correct stance for a dev/home deployment.
	RequireAuth bool `koanf:"require_auth"`
}

type SandboxConfig struct {
	AllowedPaths       []string `koanf:"allowed_paths"`
	ReadOnlyPaths      []string `koanf:"read_only_paths"`
	NetworkAllowCIDR   []string `koanf:"network_allow_cidr"`
	DangerousCmdsDeny  []string `koanf:"dangerous_cmds_deny,omitempty"`
	DangerousCmdsAllow []string `koanf:"dangerous_cmds_allow,omitempty"`
	EnvWhitelist       []string `koanf:"env_whitelist"`
	CPUQuota           int      `koanf:"cpu_quota"`
	MemoryLimitMB      int      `koanf:"memory_limit_mb"`

	// PolicyDirs overrides the default policy.d discovery chain.
	// Leave empty in almost all cases — the loader derives a sensible
	// default (user-global → config-dir → cwd). When set, the caller
	// is explicit and the defaults are NOT merged in: this is the
	// "if I set --policy-dir, don't sneak in extras" behaviour.
	// Order matters: later dirs override earlier ones on same-tool
	// conflicts. A single string in the array is equivalent to the
	// old `policy_dir` key.
	PolicyDirs []string `koanf:"policy_dirs"`

	// SkipPermChecks bypasses the policy-file integrity check. Use
	// only in environments where Unix mode/UID semantics aren't
	// reliable (certain k8s volume drivers, non-standard tmpfs).
	// Default false — on Linux the check is meaningful defence in
	// depth against a compromised tool writing policy files.
	SkipPermChecks bool `koanf:"skip_perm_checks"`

	// HotReloadOptOut disables the fsnotify policy-watcher (Phase
	// 4.5.7a-reload). Named "opt-out" so the zero-value (false)
	// gives the safe default: reload enabled. Operators setting
	// this to true want an air-gapped, load-once-at-boot deployment.
	HotReloadOptOut bool `koanf:"hot_reload_opt_out"`
}

type AuditConfig struct {
	Raft  AuditRaftConfig  `koanf:"raft"`
	Local AuditLocalConfig `koanf:"local"`
}

type AuditRaftConfig struct {
	Enabled       bool          `koanf:"enabled"`
	AnchorTarget  string        `koanf:"anchor_target"`
	AnchorCadence time.Duration `koanf:"anchor_cadence"`
}

type AuditLocalConfig struct {
	Enabled       bool          `koanf:"enabled"`
	Path          string        `koanf:"path"`
	MaxSizeMB     int           `koanf:"max_size_mb"`
	MaxFiles      int           `koanf:"max_files"`
	AnchorTarget  string        `koanf:"anchor_target,omitempty"`
	AnchorCadence time.Duration `koanf:"anchor_cadence,omitempty"`
}

type SkillsConfig struct {
	// SigningPolicy gates manifest signatures: "off" | "prefer" |
	// "require". Empty / unrecognised → "prefer" (accept both but
	// break version ties in favour of signed). Matches the
	// tri-state skills.SigningPolicy.
	SigningPolicy string `koanf:"signing_policy"`

	// TrustedPublishers is the path to a text file with one
	// "publisher-name base64-ed25519-pubkey" entry per line.
	// Loaded at boot; changes require a config reload.
	TrustedPublishers string `koanf:"trusted_publishers"`

	// RequireSigned retained for backward-compat with older configs.
	// When true (and SigningPolicy empty) the effective policy is
	// SigningRequire. Prefer SigningPolicy for new configs.
	RequireSigned bool `koanf:"require_signed"`

	// StorageLabel is the [[storage.mounts]] label where skill
	// manifests live. Registry.Watch subscribes to fsnotify
	// events on this label and re-scans on changes. Empty →
	// no watcher started; skills can still be registered
	// programmatically but won't auto-discover on drop-in.
	StorageLabel string `koanf:"storage_label,omitempty"`
}

type ObservabilityConfig struct {
	TracingExporter string `koanf:"tracing_exporter"` // "otlp" | "stdout" | "none"
	OTLPEndpoint    string `koanf:"otlp_endpoint,omitempty"`
}

// LoggingConfig covers static log settings plus a slice of
// initial filters applied at startup (slog-logfilter). Runtime
// mutation of filters happens via the logfilter package's API,
// wired through NodeService.Reload in Phase 11.
type LoggingConfig struct {
	Level   string            `koanf:"level"`  // "debug" | "info" | "warn" | "error"; empty = use --log-level flag
	Format  string            `koanf:"format"` // "auto" | "json" | "text"; empty = use --log-format flag
	Filters []LogFilterConfig `koanf:"filters"`
}

// LogFilterConfig mirrors logfilter.LogFilter minus the ExpiresAt
// field — temporary filters are set via the runtime API, not TOML.
type LogFilterConfig struct {
	Type        string `koanf:"type"`         // "<attr_name>" | "context:<key>" | "source:file" | "source:function"
	Pattern     string `koanf:"pattern"`      // glob: exact, prefix*, *suffix, *contains*
	Level       string `koanf:"level"`        // "debug" | "info" | "warn" | "error"
	OutputLevel string `koanf:"output_level"` // optional — transform the output level when filter matches
	Enabled     bool   `koanf:"enabled"`
}

type ConfigOpts struct {
	Watch      bool `koanf:"watch"`
	DebounceMs int  `koanf:"debounce_ms"`
}
