package config

import (
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Config is the top-level lobslaw configuration. Each subsystem
// validates its own slice — this layer only parses and resolves
// secret references.
type Config struct {
	Node          NodeConfig          `koanf:"node"`
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
	ConfigOpts    ConfigOpts          `koanf:"config"`
}

type NodeConfig struct {
	ID string `koanf:"id"`
}

type MemoryConfig struct {
	Enabled    bool             `koanf:"enabled"`
	RaftPort   int              `koanf:"raft_port"`
	Encryption EncryptionConfig `koanf:"encryption"`
	Snapshot   SnapshotConfig   `koanf:"snapshot"`
	Dream      DreamConfig      `koanf:"dream"`
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
	Schedule string `koanf:"schedule"`
}

type StorageConfig struct {
	Enabled bool                 `koanf:"enabled"`
	Mounts  []StorageMountConfig `koanf:"mounts"`
}

type StorageMountConfig struct {
	Label            string            `koanf:"label"`
	Type             string            `koanf:"type"`
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
}

type ComputeConfig struct {
	Enabled             bool             `koanf:"enabled"`
	Providers           []ProviderConfig `koanf:"providers"`
	Chains              []ChainConfig    `koanf:"chains"`
	DefaultChain        string           `koanf:"default_chain"`
	ComplexityEstimator string           `koanf:"complexity_estimator"`
	Budgets             BudgetsConfig    `koanf:"budgets"`
	Plugins             []PluginConfig   `koanf:"plugins"`
}

type ProviderConfig struct {
	Label        string                `koanf:"label"`
	Endpoint     string                `koanf:"endpoint"`
	Model        string                `koanf:"model"`
	APIKeyRef    string                `koanf:"api_key_ref,omitempty"`
	Capabilities []string              `koanf:"capabilities,omitempty"`
	TrustTier    types.TrustTier       `koanf:"trust_tier"`
	Pricing      types.ProviderPricing `koanf:"pricing,omitempty"`
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

type BudgetsConfig struct {
	MaxToolCallsPerTurn   int     `koanf:"max_tool_calls_per_turn"`
	MaxSpendUSDPerTurn    float64 `koanf:"max_spend_usd_per_turn"`
	MaxEgressBytesPerTurn int64   `koanf:"max_egress_bytes_per_turn"`
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
}

type GatewayChannelConfig struct {
	Type           string `koanf:"type"`
	BotTokenRef    string `koanf:"bot_token_ref,omitempty"`
	SecretTokenRef string `koanf:"secret_token_ref,omitempty"`
	TLSCert        string `koanf:"tls_cert,omitempty"`
	TLSKey         string `koanf:"tls_key,omitempty"`
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

	// InitialBootstrap should be true on exactly one node of a new
	// cluster (the first-ever start). hashicorp/raft returns
	// ErrCantBootstrap silently on subsequent starts once state exists,
	// so leaving this true on a restart is harmless.
	InitialBootstrap bool `koanf:"initial_bootstrap"`

	MTLS      MTLSConfig      `koanf:"mtls"`
	Bootstrap BootstrapConfig `koanf:"bootstrap"`
}

// MTLSConfig deliberately does NOT carry the CA private key path —
// that field exists only on the `cluster sign-node` subcommand. The
// main lobslaw binary cannot read the CA key.
type MTLSConfig struct {
	CACert   string `koanf:"ca_cert"`
	NodeCert string `koanf:"node_cert"`
	NodeKey  string `koanf:"node_key"`
}

// BootstrapConfig controls first-run CA auto-generation. Off by
// default; single-node dev convenience only.
type BootstrapConfig struct {
	AutoInit bool `koanf:"auto_init"`
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
	RequireSigned     bool   `koanf:"require_signed"`
	TrustedPublishers string `koanf:"trusted_publishers"`
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
