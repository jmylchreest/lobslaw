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
	Memory    MemoryConfig     `koanf:"memory"`
	Storage   StorageConfig    `koanf:"storage"`
	Policy    PolicyConfig     `koanf:"policy"`
	Compute   ComputeConfig    `koanf:"compute"`
	Hooks     HooksConfig      `koanf:"hooks"`
	Gateway   GatewayConfig    `koanf:"gateway"`
	Discovery DiscoveryConfig  `koanf:"discovery"`
	Cluster   ClusterConfig    `koanf:"cluster"`
	Soul      SoulLoaderConfig `koanf:"soul"`
	Auth      AuthConfig       `koanf:"auth"`
	Sandbox   SandboxConfig    `koanf:"sandbox"`
	Audit     AuditConfig      `koanf:"audit"`
	Skills    SkillsConfig     `koanf:"skills"`
	Logging   LoggingConfig    `koanf:"logging"`
	MCP       MCPConfig        `koanf:"mcp"`
	Security  SecurityConfig   `koanf:"security"`
	Secrets   SecretsConfig    `koanf:"secrets"`
	Identity  IdentityConfig   `koanf:"identity"`
	// Trace is turn tracing. Off by default.
	Trace TraceConfig `koanf:"trace"`
	// SelfLearning defaults to off — the zero value of Mode parses to
	// off, so a config that does not mention it has the capability
	// absent rather than merely unused.
	SelfLearning SelfLearningConfig `koanf:"self_learning"`
	Users        []UserConfig       `koanf:"user"`
	Binaries     []BinaryConfig     `koanf:"binary"`
	// Remotes are the hosts remote_ssh may reach. A
	// top-level repeated block like [[user]] and [[binary]], because
	// it is a property of the deployment rather than of the agent.
	Remotes []RemoteConfig `koanf:"remote"`

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

// MemoryConfig is the [memory] section: the vector + episodic store
// and the background passes that maintain it. Disabled nodes still
// parse the section but wire nothing.
type MemoryConfig struct {
	Enabled    bool             `koanf:"enabled"`
	Encryption EncryptionConfig `koanf:"encryption"`
	Snapshot   SnapshotConfig   `koanf:"snapshot"`
	Dream      DreamConfig      `koanf:"dream"`
	Session    SessionConfig    `koanf:"session"`

	// Pinned memory character caps. These blocks are rendered into
	// every system prompt, so they are a fixed tax on every request —
	// the cap is what forces them to stay curated rather than becoming
	// a second archive.
	//
	// Characters, not tokens: a character count is model-independent,
	// and a limit that moves when the tokeniser changes is not one an
	// operator can reason about. Zero takes the defaults.
	PinnedProfileChars int `koanf:"pinned_profile_chars"`
	PinnedNotesChars   int `koanf:"pinned_notes_chars"`

	// WriteApproval stages agent-initiated memory writes for approval
	// instead of letting them land. Off by default.
	//
	// hermes's key name, deliberately: the concept is theirs and an
	// operator moving between the two should not have to learn a
	// second word for it.
	//
	// Everything the agent writes is gated, not a guessed-at subset.
	// A boundary drawn around "facts about the user" has to be
	// inferred, and inference gets it wrong in both directions —
	// missing the thing you cared about, and asking about a working
	// note you did not.
	//
	// Implemented as a low-priority POLICY rule rather than a branch
	// in the tool, which is what makes the answer reusable: "for this
	// conversation" becomes a session grant, "always" mints a visible
	// and revocable rule, and an operator wanting something narrower
	// writes an ordinary rule that outranks the default.
	WriteApproval bool `koanf:"write_approval,omitempty"`
}

// SelfLearningConfig governs whether the agent may write instructions
// for itself, and what happens when it does.
//
// Three states rather than a boolean. "On" and "off" leaves no room
// for "write it down but do not act on it until I have looked", which
// is the setting most people actually want — and it is the default
// here for that reason.
type SelfLearningConfig struct {
	// Mode is "off" | "propose" | "auto". Anything unrecognised,
	// including empty, is off: a typo must never be the reason an
	// agent started following its own instructions.
	Mode string `koanf:"mode"`

	// Review trigger thresholds, measured on deliberately different
	// axes: a skill answers "was there enough WORK here", which is a
	// property of one turn, and memory answers "have we learned who
	// this person is", which only accumulates across turns.
	//
	// Zero takes the defaults (10 and 10); negative disables that
	// axis.
	ReviewSkillToolIterations int `koanf:"review_skill_tool_iterations"`
	ReviewMemoryTurnInterval  int `koanf:"review_memory_turn_interval"`

	// HistoryDepth is how many PRIOR versions of an artefact are kept
	// for rollback. Named for what it bounds: "keep_versions" does not
	// say whether the active version counts, which is the first thing
	// anybody asks. The active version is always kept and does not
	// count toward this. Zero takes the default of 10.
	//
	// Bounded because it is not free: every version lives in the log
	// and in every snapshot thereafter, on every node.
	HistoryDepth int `koanf:"history_depth"`

	// Size limits per artefact, in bytes. Text-sized on purpose — the
	// store holds instructions, not payloads, and anything genuinely
	// large belongs in storage with only its digest in the log.
	// Exceeding either fails the write and names the offending path.
	// Zero takes the defaults (256 KiB per file, 1 MiB per artefact).
	MaxArtefactFileBytes  int `koanf:"max_artefact_file_bytes"`
	MaxArtefactTotalBytes int `koanf:"max_artefact_total_bytes"`

	// Curation thresholds, in days of disuse. Zero takes the defaults
	// (30 and 90).
	//
	// StaleAfterDays marks an artefact as a candidate for archiving.
	// It keeps loading — an artefact that went out of service the
	// moment it went stale could never be used again, so archiving
	// would be a ratchet with no reprieve. Anything used inside the
	// window returns to active.
	//
	// ArchiveAfterDays is measured from last use, not from the stale
	// mark, so the two are answers to the same question rather than a
	// sum. Archiving is never deletion: everything stays restorable.
	StaleAfterDays   int `koanf:"stale_after_days"`
	ArchiveAfterDays int `koanf:"archive_after_days"`

	// ProposalExpiryDays bounds the review queue: a proposal nobody
	// has looked at is archived, with reason "unreviewed" so the
	// archive can tell it apart from something somebody declined.
	// Zero takes the default of 30; NEGATIVE disables it.
	//
	// An uncomfortable setting, deliberately configurable. Expiring a
	// proposal converts "not reviewed yet" into a decision nobody
	// made — but an unbounded inbox is not an inbox, and a queue of
	// two hundred is one nobody will ever work through, at which point
	// the review fork is writing into something that functions as
	// /dev/null.
	ProposalExpiryDays int `koanf:"proposal_expiry_days"`

	// Notify controls the in-channel nudge that says a review queue
	// exists. Off unless both allowlists are populated.
	Notify NotifyConfig `koanf:"notify"`
}

// TraceConfig is turn tracing: what a turn did, and what it cost.
//
// Off by default. Enabling it writes newline-delimited JSON under the
// data dir, per node, readable with `lobslaw trace`.
//
// Per node rather than replicated, deliberately. A trace is
// high-volume, short-lived and not agreed-upon state, so putting it in
// raft would drag telemetry into the consensus path. The honest cost
// is that a turn served on one node is not queryable from another —
// the trace is local because the turn was.
//
// NO SPAN CARRIES CONTENT: no message text, no tool arguments, no tool
// output. Sizes, counts, timings, names and provider labels only.
type TraceConfig struct {
	// Enabled turns on the local file sink.
	Enabled bool `koanf:"enabled,omitempty"`

	// Dir overrides where traces land. Empty puts them under the data
	// dir, alongside the other per-node disposable state.
	Dir string `koanf:"dir,omitempty"`

	// MaxBytes bounds one trace file before it rotates; exactly one
	// predecessor is kept, so the ceiling is twice this. Zero takes
	// the default of 64 MiB.
	//
	// Bounded because an unbounded telemetry file on a long-running
	// node is a disk-full incident waiting for a quiet week.
	MaxBytes int64 `koanf:"max_bytes,omitempty"`

	// OTLPEndpoint exports to an OpenTelemetry collector, in addition
	// to the local file rather than instead of it.
	//
	// In addition, deliberately. The file is the record; the collector
	// is where you look. A collector going down must not lose the
	// trace of the turn that was failing while it was down, which is
	// exactly the trace anybody would want afterwards.
	//
	// Empty disables it. host:port, gRPC.
	OTLPEndpoint string `koanf:"otlp_endpoint,omitempty"`

	// OTLPInsecure disables TLS to the collector. Named for what it
	// does rather than "secure = false", so the config reads as an
	// admission. Spans carry no content, but they do carry provider
	// names, timings and costs.
	OTLPInsecure bool `koanf:"otlp_insecure,omitempty"`

	// ServiceName identifies this deployment in the collector. Empty
	// takes "lobslaw".
	ServiceName string `koanf:"service_name,omitempty"`
}

// NotifyConfig is the operator's opt-in to being told, in-channel,
// that something is waiting for them.
//
// The nudge rides out on a turn the user is already having — no push
// mechanism, no per-channel addressing, no delivery guarantees to get
// wrong. Any channel that can send a reply can carry it, which is what
// makes adding a channel later configuration rather than code.
//
// Two allowlists rather than a boolean, and BOTH must match. Channels
// decides where; subjects decides who, and that cannot be inferred —
// the person a notice concerns is the one who can act on it, and
// nothing in the conversation says who that is. A channel allowlist on
// its own would tell a group chat what the operator has pending.
//
// Example:
//
//	[self_learning.notify]
//	channels = ["telegram"]
//	subjects = ["user:tg-@john"]
//	interval = "24h"
type NotifyConfig struct {
	// Disabled turns the nudge off in propose mode.
	//
	// An opt-OUT, because mode = "propose" is already the statement
	// that a human should look before anything the agent wrote takes
	// effect. Requiring a second, separately-populated block to hear
	// about the queue made "propose" mean "write to a queue nobody is
	// told about" — which is auto mode with extra steps, and worse,
	// because proposal expiry then discards things nobody declined.
	//
	// Ignored in auto and off modes, where there is no queue to
	// nudge about.
	Disabled bool `koanf:"disabled,omitempty"`

	// Channels that may carry notices, by gateway kind ("telegram",
	// "rest"). Empty in propose mode takes every configured gateway
	// channel — a notice about a queue is for whoever can reach the
	// assistant, and naming them again here is a second list to keep
	// in step with the first.
	Channels []string `koanf:"channels,omitempty"`

	// Subjects that may receive them, as principals. Empty in propose
	// mode takes the owner-scoped users from the channel config: the
	// people already trusted to approve things are the people to tell
	// there is something to approve.
	Subjects []string `koanf:"subjects,omitempty"`

	// Interval is the minimum gap between notices in one conversation.
	// Zero takes the default of 24h — a nudge appended to every turn
	// stops being information within an hour, after which it is read
	// past, which is worse than never having sent it.
	Interval time.Duration `koanf:"interval,omitempty"`
}

// IdentityConfig maps the per-channel user ids lobslaw receives onto
// cluster-wide principals.
//
// Only needed when one person reaches the node under more than one id
// — the usual case being the same human on Telegram and over REST.
// Without it every channel id is its own principal, which is correct
// but means that person will not find their Telegram history from a
// REST session, and memories written on one channel are not visible
// from the other.
type IdentityConfig struct {
	// Aliases maps a channel user id to a canonical id:
	//
	//	[identity.aliases]
	//	"tg-@alice"         = "alice"
	//	"alice@example.com" = "alice"
	//
	// Values are bare ids — lobslaw prefixes the principal kind
	// itself, so a config typo cannot mint a principal kind nothing
	// else understands. Keys match case-insensitively.
	Aliases map[string]string `koanf:"aliases"`
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

// EncryptionConfig is [memory.encryption]. KeyRef is a secret
// reference (not the key itself) resolved via ResolveSecret at boot;
// memory records are encrypted at rest under it.
type EncryptionConfig struct {
	KeyRef string `koanf:"key_ref"`
}

// SnapshotConfig is [memory.snapshot]. Target names a storage mount
// to ship raft snapshots to.
//
// Shipping is not implemented yet — Target's only current effect is
// on boot validation, where a memory node with neither a snapshot
// target nor seed nodes is a single point of loss. cadence and
// retention were removed: they described a schedule and a pruning
// policy for a job that does not run.
type SnapshotConfig struct {
	Target string `koanf:"target"`
}

// DreamConfig is [memory.dream], governing the REM-sleep
// consolidation pass that folds many episodic records into summaries.
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

// StorageConfig is the [storage] section: the object store and the
// set of mounts exposed to the agent as a sandboxed filesystem.
type StorageConfig struct {
	Enabled bool                 `koanf:"enabled"`
	Mounts  []StorageMountConfig `koanf:"mounts"`
}

// StorageMountConfig is one [[storage.mounts]] entry. Type selects
// the backend ("local", "s3", "minio", "r2"); the credential and
// bucket fields below are backend-specific and ignored by backends
// that do not use them.
type StorageMountConfig struct {
	Label string `koanf:"label"`
	Type  string `koanf:"type"`
	Path  string `koanf:"path,omitempty"`
	// Mode is the access bits granted to the agent for this mount,
	// expressed as any subset of "rwx" (case-insensitive). Empty or
	// "r"/"ro" → read-only (the safe default). "rw" enables write,
	// "rx" enables exec (binaries the agent may run but not modify),
	// "rwx" enables all three. Mount mode is the single source of
	// truth — the MountResolver gates fs builtins by it AND the
	// Landlock helper builds skill / shell_command sandboxes from
	// the same table.
	Mode string `koanf:"mode,omitempty"`
	// Excludes is a list of glob patterns (e.g. ".git/**", "*.key",
	// "node_modules/**") hidden from list/glob/grep/read inside
	// this mount. Hardcoded internal excludes (.snapshot, state.db,
	// *.pem) always apply on top of these.
	Excludes []string `koanf:"excludes,omitempty"`
	Bucket   string   `koanf:"bucket,omitempty"`
	// Server and Export address an NFS mount. Required for
	// type = "nfs" — the backend errors without them, and like Remote
	// below there was no key to set them with, so an NFS mount
	// declared in TOML could not start either.
	Server string `koanf:"server,omitempty"`
	Export string `koanf:"export,omitempty"`
	// Remote is the rclone remote name, as configured in rclone's own
	// config. Required for type = "rclone": the backend errors without
	// it, which until now made an rclone mount declared in TOML
	// impossible to start, because there was no key to set it with.
	Remote string `koanf:"remote,omitempty"`
	// Options is passed to the backend verbatim. Keys ending in "_ref"
	// are resolved as secret references before the backend starts;
	// everything else is handed over as-is.
	//
	// One map rather than a field per credential. The previous shape
	// had endpoint / account / access_key_ref / secret_key_ref /
	// crypt_password_ref / crypt_salt_ref / env / extra_opts — eight
	// keys in a vocabulary the backend does not speak, none of which
	// reached it. A backend that grows an option should not require a
	// config change to accept one.
	Options map[string]string `koanf:"options,omitempty"`
}

// PolicyConfig is the [policy] section: whether this node evaluates
// policy, plus the rules seeded into raft at boot.
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

// PolicyRuleConfig is one [[policy.rules]] entry, mirroring the
// fields of lobslawv1.PolicyRule. See PolicyConfig.Rules for the
// subject-format and priority rules that apply here.
type PolicyRuleConfig struct {
	ID       string `koanf:"id"`
	Subject  string `koanf:"subject"`
	Action   string `koanf:"action"`
	Resource string `koanf:"resource"`
	Effect   string `koanf:"effect"`             // "allow" | "deny"
	Priority int32  `koanf:"priority,omitempty"` // higher wins
}

// ComputeConfig is the [compute] section: LLM providers, the chains
// that route between them, per-turn limits, and the plugin and
// modality settings layered on top.
type ComputeConfig struct {
	Enabled      bool             `koanf:"enabled"`
	Providers    []ProviderConfig `koanf:"providers"`
	Chains       []ChainConfig    `koanf:"chains"`
	DefaultChain string           `koanf:"default_chain"`
	Budgets      BudgetsConfig    `koanf:"budgets"` // deprecated; use Limits
	Limits       LimitsConfig     `koanf:"limits,omitempty"`
	WebSearch    WebSearchConfig  `koanf:"web_search,omitempty"`
	// SearchProviders are the declared web-search backends,
	// [[compute.search_providers]]. Separate from Providers because a
	// search engine is not an LLM endpoint: it has no model, no token
	// pricing, and no capability discovery, so folding it into
	// ProviderConfig would mean a struct where two thirds of the fields
	// never apply.
	SearchProviders []SearchProviderConfig `koanf:"search_providers,omitempty"`
	// DisabledTools are glob patterns matched against tool NAMES. A
	// matching tool is never registered, so the agent does not see it
	// and cannot call it — a stronger gate than a policy deny, which
	// still puts the tool in the model's list and then refuses it.
	//
	// A POINTER because unset and empty mean opposite things. Absent
	// takes compute.DefaultDisabledTools, which switches the remote_*
	// family off; `disabled_tools = []` is how an operator says "I
	// want all of them", deliberately and in writing. A plain slice
	// could not tell those apart, and the one that would silently win
	// is the permissive one.
	//
	//	disabled_tools = ["remote_*", "shell_command"]
	//	disabled_tools = []              # nothing disabled, including remote_*
	//	disabled_tools = ["remote_scp"]  # remote_ssh on, remote_scp off
	DisabledTools *[]string `koanf:"disabled_tools,omitempty"`

	Vision     VisionConfig     `koanf:"vision,omitempty"`
	Audio      AudioConfig      `koanf:"audio,omitempty"`
	PDF        PDFConfig        `koanf:"pdf,omitempty"`
	Embeddings EmbeddingsConfig `koanf:"embeddings,omitempty"`
	Speak      SpeakConfig      `koanf:"speak,omitempty"`
	Image      ImageGenConfig   `koanf:"image,omitempty"`
	Video      VideoGenConfig   `koanf:"video,omitempty"`

	// ArtifactMount names the storage mount that receives generated
	// files (speech, images, video). Empty falls back to the first
	// writable mount, which is convenient for a single-mount
	// deployment and ambiguous for any other — declare it explicitly
	// once there is more than one place a file could land.
	ArtifactMount string `koanf:"artifact_mount,omitempty"`

	// ArtifactDiskPercent bounds what generated media may occupy, as a
	// share of the filesystem the artifact mount sits on.
	//
	// A percentage rather than a byte cap because the right number
	// depends entirely on the volume: 500MB is nothing on a 4TB array
	// and most of a container's scratch space, and an operator should
	// not have to restate that for every deployment.
	//
	// Zero takes the default (5%). NEGATIVE DISABLES SWEEPING, which is
	// a real choice for a node whose mount is already managed by
	// something else — and is spelled as a negative number rather than
	// a separate bool so "off" cannot be reached by leaving a field
	// unset and assuming it meant unlimited.
	//
	// Oldest first, and only under generated/. Nothing else in the
	// mount is reachable by the sweep.
	ArtifactDiskPercent float64 `koanf:"artifact_disk_percent,omitempty"`
	// Roles maps named functional roles (main, preflight,
	// reranker, summariser, etc.) to provider labels. Internal
	// code asks the resolver for a role by name; the resolver
	// dereferences to the provider. Empty → first provider fills
	// every role (today's behaviour).
	Roles RolesConfig `koanf:"roles,omitempty"`

	// Context bounds how much prior conversation is replayed into
	// each turn. Omitted → compute.DefaultContextBudget().
	Context ContextConfig `koanf:"context,omitempty"`
}

// ContextConfig tunes per-turn context assembly.
//
// Without bounds, a conversation's cost is quadratic in its length —
// every turn re-sends every previous turn, and a replayed tool result
// can be as large as Executor.MaxOutputBytes (10 MB). These two knobs
// make it roughly flat.
// ContextConfig is [compute.context].
//
// Three settings were removed here rather than kept and validated:
// tail_messages, compact_max_completion_tokens and title_max_chars.
//
// The first two were second caps on things another setting already
// capped, and a second cap can contradict the first — a tail_messages
// of 5 alongside a generous tail_tokens truncates history for a reason
// the operator cannot see, because the tighter of two caps wins
// silently. They are now DERIVED from the setting that carries the
// meaning. A validation error would tell an operator they got it
// wrong; deriving means they cannot.
//
// title_max_chars was a UI constant wearing a policy's clothes. A
// title's length does not vary by deployment, and a channel that wants
// them shorter can shorten them.
type ContextConfig struct {
	// TailTokens caps the estimated tokens of replayed history per
	// turn; oldest messages drop first. Explicit 0 = unbounded
	// (the pre-budget behaviour — expensive, and eventually fatal
	// on long conversations).
	TailTokens *int `koanf:"tail_tokens,omitempty"`

	// HistoryToolResultBytes truncates REPLAYED tool results to this
	// many bytes. The turn that produced a result always sees it in
	// full. Explicit 0 = replay tool output untouched.
	HistoryToolResultBytes *int `koanf:"history_tool_result_bytes,omitempty"`

	// CompactKeepMessages is how many recent messages stay verbatim.
	// Compaction only summarises what is older, so the immediate
	// exchange is never replaced by prose.
	CompactKeepMessages *int `koanf:"compact_keep_messages,omitempty"`

	// CompactTriggerTokens is how much aged-out conversation must
	// accumulate before a summariser call is worth making. Explicit
	// 0 uses the default; compaction is disabled by leaving the
	// summariser role unset, not by zeroing this.
	CompactTriggerTokens *int `koanf:"compact_trigger_tokens,omitempty"`

	// CompactMaxSummaryTokens caps the running summary. It rides on
	// every subsequent turn, so an unbounded summary recreates the
	// problem compaction exists to solve.
	CompactMaxSummaryTokens *int `koanf:"compact_max_summary_tokens,omitempty"`

	// CompactEnabled turns compaction off without unsetting the
	// summariser role (which other subsystems also use). Unset =
	// enabled whenever a summariser resolves.
	CompactEnabled *bool `koanf:"compact_enabled,omitempty"`

	// CompactToolResultBytes is how much of each tool result the
	// summariser sees. The summariser exists to save tokens; feeding
	// it megabytes of grep output defeats that on the very call
	// meant to help.
	CompactToolResultBytes *int `koanf:"compact_tool_result_bytes,omitempty"`

	// TitlesEnabled turns title generation off while leaving
	// compaction on. Titles cost one extra small call per
	// conversation, once.
	TitlesEnabled *bool `koanf:"titles_enabled,omitempty"`

	// SessionSearchResults, SessionSearchSnippets and
	// SessionReadMessages bound what the session_* tools may pull
	// into the agent's context. Unbounded, one session_read would
	// undo the context budget in a single tool call.
	SessionSearchResults  *int `koanf:"session_search_results,omitempty"`
	SessionSearchSnippets *int `koanf:"session_search_snippets,omitempty"`
	SessionReadMessages   *int `koanf:"session_read_messages,omitempty"`

	// CompactInstructions are appended to the built-in summariser
	// prompt — use it to name what this deployment must never lose
	// ("always keep schema decisions and ticket numbers").
	//
	// Appended rather than replacing: the built-in prompt encodes
	// the difference between a summary that records decisions and
	// one that narrates topics, and losing that silently degrades
	// every future turn.
	CompactInstructions string `koanf:"compact_instructions,omitempty"`
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

// WebSearchConfig selects which declared search backends the
// web_search builtin uses. When it resolves to nothing, the builtin is
// not registered and the model sees no web_search tool — a deployment
// that wants no web access sets nothing rather than redacting
// anything. MCP-sourced web_search registrations (future) override the
// builtin by virtue of later-registration wins in the tool registry.
type WebSearchConfig struct {
	// Providers names [[compute.search_providers]] labels in
	// preference order. More than one is a failover chain: the second
	// is tried when the first fails transiently, which is what makes
	// "self-hosted first, hosted API as a safety net" configuration
	// rather than code.
	Providers []string `koanf:"providers,omitempty"`

	// Provider is sugar for a one-element Providers. It exists because
	// the single-backend case is the common one and because the config
	// reference documented this key long before it worked.
	Provider string `koanf:"provider,omitempty"`

	// APIKeyRef and Endpoint are the pre-driver shape, kept working.
	// A config carrying only these means Exa at the given endpoint,
	// which is exactly what it meant before search backends were
	// pluggable — nobody's config breaks on upgrade.
	APIKeyRef string `koanf:"api_key_ref,omitempty"`
	Endpoint  string `koanf:"endpoint,omitempty"`
}

// SearchProviderConfig is one declared web-search backend.
//
// The three maps are what keep adding a backend cheap. Search APIs
// differ almost entirely in parameter names and response paths, so
// typing each vendor's knobs into this struct would make every new
// engine a struct change and a rebuild. With `driver = "template"`,
// Options/ExtraParams/Response describe the whole API and no Go is
// written at all; a compiled driver reads Options for its own knobs
// and ignores the rest. Each driver validates its own keys at boot, so
// a typo fails on start-up naming the offending key.
type SearchProviderConfig struct {
	Label string `koanf:"label"`

	// Driver names the backend implementation: "exa" (the default,
	// and what every pre-driver config meant), "searxng", or
	// "template" for a provider described entirely by the fields
	// below.
	Driver string `koanf:"driver,omitempty"`

	Endpoint  string `koanf:"endpoint,omitempty"`
	APIKeyRef string `koanf:"api_key_ref,omitempty"`

	// TrustTier is checked against the soul's min_trust_tier before
	// this backend runs. It matters more here than it looks: a search
	// hands the user's own words to whoever answers, so a self-hosted
	// SearXNG declaring "local" and a hosted API declaring "public"
	// are describing a real difference.
	TrustTier types.TrustTier `koanf:"trust_tier,omitempty"`

	// Timeout bounds one query. Zero takes the driver default (15s) —
	// a search is interactive, so it is far tighter than the minute a
	// model round-trip is allowed.
	Timeout time.Duration `koanf:"timeout,omitempty"`

	Options     map[string]string `koanf:"options,omitempty"`
	ExtraParams map[string]string `koanf:"extra_params,omitempty"`
	Response    map[string]string `koanf:"response,omitempty"`
}

// ModalityOverride pins a modality builtin to one specific provider
// label, bypassing capability auto-discovery. Empty Provider →
// auto-discovery picks from [[compute.providers]] entries tagged
// with the matching capability (highest Priority wins). Operators
// only need this when they have multiple capability-matching
// providers and want a non-priority pick for a specific modality.
//
// VisionConfig, AudioConfig and PDFConfig below are thin shells over
// this type. 99% of operators leave them empty — declaring a
// provider with capabilities = ["vision"] (etc) is enough.
type ModalityOverride struct {
	Provider string `koanf:"provider,omitempty"`
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
type VisionConfig = ModalityOverride

// SpeakConfig selects the provider backing the speak (text-to-speech)
// builtin. Same shape as the other modality overrides: naming a
// provider pins to it, and leaving it empty discovers providers by
// the "speak" capability tag in priority order.
type SpeakConfig = ModalityOverride

// ImageGenConfig selects the provider backing the generate_image
// builtin. Named ImageGen because VisionConfig already covers reading
// images; this one writes them.
type ImageGenConfig = ModalityOverride

// VideoGenConfig selects the provider backing generate_video.
type VideoGenConfig = ModalityOverride

// AudioConfig selects the provider backing the audio modality
// builtins (transcription of inbound voice notes and audio files).
type AudioConfig = ModalityOverride

// PDFConfig selects the provider backing the PDF modality builtin,
// which extracts text from documents the agent is handed a path to.
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
//
// CHOOSING A MODEL. Vectors outlive the config that made them, so
// this is close to a one-way door: cosine between two models'
// vectors is meaningless, and changing Model is therefore refused
// at boot (see memory.CheckEmbeddingModel) until the corpus is
// re-embedded with `backfill-embeddings --force`.
//
// Prefer an OPEN-WEIGHT model over a vendor-proprietary one. The
// reason is portability of the corpus rather than licence
// principle: a proprietary model reachable from exactly one vendor
// means that vendor's deprecation schedule decides when you
// re-embed everything. Open weights mean the worst case is
// self-hosting, and the vectors stay valid.
//
// By the models.dev catalog this node already caches, the widest
// availability by some distance is Qwen3-Embedding — 11 providers
// carry some size of it, against 5 for OpenAI's text-embedding-3
// and 5 for Gemini's. qwen3-embedding-8b alone is on 7.
//
// That catalog under-reports embeddings, though — it is primarily a
// chat-model registry, and it lists none for MiniMax even though
// embo-01 is named below. So treat it as a floor on availability,
// not a census. Two consequences worth knowing: OpenRouter serves
// no embeddings at all (352 chat models, zero embedding), so an
// openrouter role cannot cover this; and a provider's absence from
// the catalog is not evidence it lacks an endpoint.
type EmbeddingsConfig struct {
	// Type selects where embeddings come from.
	//
	//	""        — same as "remote"; every existing config keeps working
	//	"remote"  — an HTTP endpoint, using Endpoint/APIKeyRef/Format
	//	"builtin" — a model file this node runs itself, in-process
	//
	// "builtin" is the option with no API key, no endpoint, and no
	// egress at query time: memory content never leaves the node. That
	// matters more here than it would elsewhere, because embeddings are
	// computed for EVERY record — including PRIVATE ones — so a remote
	// embedder is a standing disclosure of the whole corpus to a third
	// party, not an occasional one.
	Type string `koanf:"type,omitempty"`

	// Endpoint and APIKeyRef are for Type "remote" only.
	Endpoint  string `koanf:"endpoint,omitempty"`
	APIKeyRef string `koanf:"api_key_ref,omitempty"`
	Format    string `koanf:"format,omitempty"`

	// Model names the embedding model.
	//
	// For "remote" it is the vendor's model string. For "builtin" it is
	// a directory name under <data_dir>/models, and also the identity
	// stamped on every vector — see memory.CheckEmbeddingModel, which
	// refuses to start when it disagrees with the corpus.
	Model string `koanf:"model,omitempty"`

	// DownloadURL is where a "builtin" model is fetched from when it is
	// not already cached: the base of an HTTP directory holding
	// config.json, model.safetensors and tokenizer.json.
	//
	// Empty means the model must already be present on disk. That is
	// the stricter setting and the right one for an air-gapped node —
	// nothing is fetched, and a missing model is an error at boot
	// rather than a download at first use.
	DownloadURL string `koanf:"download_url,omitempty"`

	// Dims is the model's output width.
	//
	// Required for "remote", where nothing can verify it until the
	// first call fails. Optional for "builtin": the width is read from
	// the checkpoint, and a value given here is CHECKED against it
	// rather than trusted.
	Dims int `koanf:"dims,omitempty"`

	// QueryPrefix and PassagePrefix are prepended to text before
	// embedding, for models trained ASYMMETRICALLY.
	//
	// The e5 family is trained with "query: " on the question and
	// "passage: " on the stored text, and applying neither costs
	// measurable recall — one hit at both recall@1 and recall@3 on a
	// twenty-query set.
	//
	// Empty by default, and CORRECTLY empty for the recommended
	// model: all-MiniLM-L6-v2 is symmetric and prefixing it would make
	// things worse. There is nothing in a checkpoint that declares
	// which it is, so this is configuration rather than detection —
	// guessing from the model name would be wrong the first time
	// somebody renamed a directory.
	QueryPrefix   string `koanf:"query_prefix,omitempty"`
	PassagePrefix string `koanf:"passage_prefix,omitempty"`
}

// Builtin reports whether embeddings are computed in-process.
func (e EmbeddingsConfig) Builtin() bool { return e.Type == "builtin" }

// Configured reports whether any embedder is set up at all.
//
// Absence is a supported configuration, not a broken one: recall falls
// back to lexical matching, which is what a node with no [embeddings]
// block has always done.
func (e EmbeddingsConfig) Configured() bool {
	return e.Builtin() || e.Endpoint != ""
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
	Label string `koanf:"label"`
	// Driver names the wire protocol this endpoint speaks:
	// "openai" (the default, and what most vendors offer),
	// "anthropic", or "mock" for a node that must not touch the
	// network. Empty means openai, so every existing config keeps
	// working.
	//
	// A driver is not a vendor. Qwen Cloud, Groq, Together and a
	// local Ollama are all providers using the openai driver at
	// different endpoints — which is why the list of drivers stays
	// short while the list of providers does not.
	Driver   string `koanf:"driver,omitempty"`
	Endpoint string `koanf:"endpoint"`
	Model    string `koanf:"model"`
	Format   string `koanf:"format,omitempty"`
	Priority int    `koanf:"priority,omitempty"`
	// AutoCapabilities turns on models.dev capability discovery for
	// this provider entry. At node boot the catalog is fetched (24h
	// disk cache), the configured model is looked up, and the
	// discovered modalities are MERGED with declared capabilities.
	// Declared capabilities always win on conflict. Off by default.
	AutoCapabilities bool `koanf:"auto_capabilities,omitempty"`
	// AutoPricing fills this provider's rate card from the same
	// catalogue, when no pricing block is declared. Off by default,
	// and separate from AutoCapabilities because they fail
	// differently: a wrong capability breaks a feature loudly, a
	// wrong price misreports money quietly.
	//
	// Declared pricing always wins, whole-block. The catalogue quotes
	// per MILLION tokens and lobslaw per thousand; the conversion
	// lives in one named function so the factor is not retyped.
	AutoPricing  bool                  `koanf:"auto_pricing,omitempty"`
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

// ChainConfig is one [[compute.chains]] entry — an ordered pipeline
// of provider/role steps selected by Trigger. MinTrustTier refuses
// the chain when the resolved provider sits below the floor.
type ChainConfig struct {
	Label        string             `koanf:"label"`
	Steps        []ChainStepConfig  `koanf:"steps"`
	Trigger      ChainTriggerConfig `koanf:"trigger"`
	MinTrustTier types.TrustTier    `koanf:"min_trust_tier,omitempty"`
}

// ChainStepConfig is one step of a chain: which provider label to
// call and in which role, optionally with a prompt template that
// overrides the role default.
type ChainStepConfig struct {
	Provider       string `koanf:"provider"`
	Role           string `koanf:"role"`
	PromptTemplate string `koanf:"prompt_template,omitempty"`
}

// ChainTriggerConfig decides whether a chain matches a request.
// Always wins outright; otherwise MinComplexity and Domains both
// have to be satisfied, and a trigger with neither set never matches
// automatically (use default_chain for the fallback).
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
	MaxSpendUSDPerTurn    float64 `koanf:"max_spend_usd_per_turn,omitempty"`    // deprecated: no-op
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
// (env:, file:, or a [[secrets.providers]] label) the same way every
// other lobslaw secret does.
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

// [[compute.plugins]] used to live here — name, source, enabled,
// auto_install_binary — and nothing read any of it. Plugins are
// installed with `lobslaw plugin install`, which writes into the
// skills root; declaring them again in TOML was a second authority
// for the same thing, and the one nobody had wired up.

// HooksConfig is keyed by event name (PreToolUse, PostToolUse, …).
// Each event may have multiple subprocess hooks.
type HooksConfig map[string][]types.HookConfig

// GatewayConfig is the [gateway] section: the inbound listeners and
// the channels mounted on them, plus the defaults applied to turns
// those channels dispatch.
type GatewayConfig struct {
	Enabled             bool                   `koanf:"enabled"`
	HTTPPort            int                    `koanf:"http_port"`
	Channels            []GatewayChannelConfig `koanf:"channels"`
	ConfirmationTimeout time.Duration          `koanf:"confirmation_timeout"`
	UnknownUserScope    string                 `koanf:"unknown_user_scope"`

	// HTTP server timers for the REST listener. Zero takes a default.
	//
	// WriteTimeout is the one that bites. It bounds the whole
	// request-to-response window, so a value below HardTimeout kills
	// the socket before the agent's own cap can produce the
	// forced-summary reply — the caller sees "Empty reply from server"
	// on a turn that completed server-side and wrote its artifacts.
	// Left unset it is derived from HardTimeout rather than fixed, so
	// raising one does not silently require raising the other.
	ReadTimeout  time.Duration `koanf:"read_timeout,omitempty"`
	WriteTimeout time.Duration `koanf:"write_timeout,omitempty"`
	IdleTimeout  time.Duration `koanf:"idle_timeout,omitempty"`

	// DefaultTimezone is the cluster-wide IANA zone used when a user
	// hasn't bound a per-user timezone via [[user]] config or the
	// future timezone-binding builtin. Empty = "UTC". Stored UTC
	// times are CONVERTED to this zone for display in agent
	// responses, scheduled-task descriptions, commitment due_at, etc.
	DefaultTimezone string `koanf:"default_timezone,omitempty"`

	// IncomingDir is where channel handlers materialise inbound
	// attachments, and the only directory read_image / read_audio /
	// read_pdf will open a path in. Empty → "/workspace/incoming".
	//
	// Configurable because the default only exists inside the
	// container image. On a host install nothing creates /workspace,
	// os.MkdirAll under it fails for an unprivileged process, and the
	// result is that sending the bot a photograph fails AND the agent
	// can never read one — with the refusal naming a path the
	// operator has no way to change.
	//
	// ONE key for both halves. The handler writes here and the vision
	// builtin only reads here; two settings that had to agree would
	// eventually not, and the failure would be a file that exists
	// where nothing is allowed to look at it.
	IncomingDir string `koanf:"incoming_dir,omitempty"`

	// SessionMaxMessages caps how many messages each conversation
	// transcript retains in the durable session store. Trimming drops
	// the oldest first. Zero takes memory.DefaultSessionMaxMessages.
	//
	// This is a storage bound, not a context bound — raising it costs
	// disk and raft-snapshot size, not tokens per turn.
	SessionMaxMessages int `koanf:"session_max_messages,omitempty"`

	// SessionCacheMessages and SessionCacheTTL tune the in-memory
	// conversation buffer that fronts the durable store. It is the
	// degraded mode for turns handled on a raft follower (session
	// writes are leader-only), not a performance cache — raise it if
	// followers routinely serve long conversations. Zero takes the
	// defaults (100 messages / 30m).
	SessionCacheMessages int           `koanf:"session_cache_messages,omitempty"`
	SessionCacheTTL      time.Duration `koanf:"session_cache_ttl,omitempty"`

	// QueueMode decides what happens to a message that arrives while
	// a turn is already running for the same conversation. One of
	// "serial" (default), "latest", "debounce", "off" — see
	// gateway.QueueMode.
	//
	// This is a correctness setting before it is a UX one: without
	// serialisation two messages during one turn both read the same
	// prior history and both append it, interleaving the transcript.
	// The modes differ only in what happens to the second message,
	// never in whether the turns overlap.
	QueueMode string `koanf:"queue_mode,omitempty"`

	// QueueDebounce is the fold window for queue_mode = "debounce".
	// Zero takes gateway.DefaultDebounce (3s). Ignored in the other
	// modes.
	QueueDebounce time.Duration `koanf:"queue_debounce,omitempty"`

	// QueueBurstWindow is the window a conversation EARNS by having a
	// message arrive while it was busy, under queue_mode = "smart".
	//
	// smart starts instant, because the window is the entire latency
	// cost of these modes: measured on one cluster a lone message took
	// 1.5s with no window and 7.4s at five seconds, and lone messages
	// are the common case. A window is opened only for people who have
	// shown they type in bursts.
	//
	// Zero takes the default (the same as queue_debounce's); negative
	// disables the learning, leaving queue_debounce as the only
	// window.
	QueueBurstWindow time.Duration `koanf:"queue_burst_window,omitempty"`

	// QueueBurstReset is how long a conversation must go without
	// bursting before the earned window decays back to instant.
	// Zero takes the default of five minutes.
	QueueBurstReset time.Duration `koanf:"queue_burst_reset,omitempty"`

	// Responsiveness timers. Zero on any = disabled. Operators can
	// tune per deployment; sensible defaults land in Load().
	TypingInterval time.Duration `koanf:"typing_interval"` // refresh typing indicator (Telegram clears at ~5s)
	InterimTimeout time.Duration `koanf:"interim_timeout"` // send "still working" message after this (chatty SOUL only)
	HardTimeout    time.Duration `koanf:"hard_timeout"`    // cancel turn + force summary reply after this
}

// UserConfig is the operator-declared per-user binding. Seeded into
// BucketUserPrefs at boot if the bucket doesn't already hold a
// record for the same id (operator config is the source of truth on
// first boot; runtime edits via builtins win on subsequent boots).
//
// Solo deployments declare one entry: id="owner". Team / corporate
// deployments declare one per human, each with their channel
// addresses + timezone preference.
type UserConfig struct {
	ID          string                  `koanf:"id"`
	DisplayName string                  `koanf:"display_name,omitempty"`
	Timezone    string                  `koanf:"timezone,omitempty"`
	Language    string                  `koanf:"language,omitempty"`
	Channels    []UserChannelAddrConfig `koanf:"channels,omitempty"`

	// Roles are the policy subjects this person holds, matched by
	// rules written as subject = "role:operator". They exist here
	// because a JWT is the only other way to assert a role, and most
	// channels have no JWT: someone talking to the bot over Telegram
	// arrives with a chat id and nothing else, so without an
	// operator-declared list there is no way to say who they are
	// beyond their name.
	//
	// Holding a role grants nothing on its own. It only makes the
	// principal matchable by a rule, so what the role can do stays a
	// property of the policy the operator wrote rather than of the
	// string "operator" appearing in the code.
	Roles []string `koanf:"roles,omitempty"`
}

// UserChannelAddrConfig binds one (channel, address) pair for a user.
// Type is the gateway channel kind ("telegram", "rest", "slack");
// Address is the channel-specific identifier (Telegram chat_id as
// string, REST claims subject, Slack user id).
type UserChannelAddrConfig struct {
	Type    string `koanf:"type"`
	Address string `koanf:"address"`
}

// GatewayChannelConfig is one [[gateway.channels]] entry. Type
// selects the channel implementation ("telegram", "webhook", …) and
// determines which of the fields below are consulted.
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
	Name            string `koanf:"name,omitempty"`
	WebhookPath     string `koanf:"webhook_path,omitempty"`
	SharedSecretRef string `koanf:"shared_secret_ref,omitempty"`
	Scope           string `koanf:"scope,omitempty"`

	// Slack channel fields. Only consulted when Type == "slack".
	//
	// AppTokenRef is the app-level token ("xapp-…") that opens a
	// Socket Mode connection; BotTokenRef above is the bot token
	// ("xoxb-…") every Web API call authenticates with. Two tokens,
	// two jobs — Socket Mode cannot be opened with a bot token and
	// chat.postMessage cannot be called with an app token.
	AppTokenRef string `koanf:"app_token_ref,omitempty"`

	// AllowedChannels lists the Slack channel ids this bot will act
	// in. ["*"] is wide open. EMPTY IS CLOSED: an operator who has not
	// said where the bot may speak has not thereby said "anywhere".
	//
	// "dm" is a sentinel matching every direct message, because a D-id
	// is minted per user on first contact and cannot be written down in
	// advance. allowed_channels = ["dm", "C0123ABC"] is the shape most
	// deployments want: anyone may DM the assistant, and it speaks in
	// exactly one channel.
	//
	// It gates two different things and must be consulted for both —
	// which conversations produce turns, and which conversations the
	// slack_* read tools may fetch. Enforcing it only on inbound
	// events would govern what the agent HEARS while leaving what it
	// can GO AND READ wide open.
	AllowedChannels []string `koanf:"allowed_channels,omitempty"`
}

// SecretsConfig is the [secrets] section: where secret references
// other than env: and file: resolve from.
type SecretsConfig struct {
	// Providers are the declared vaults. A provider's LABEL is the
	// reference scheme it answers to, so a provider labelled "bw" makes
	// "bw:app/key" resolvable anywhere "env:APP_KEY" works today.
	Providers []SecretProviderConfig `koanf:"providers,omitempty"`

	// CacheTTL is how long a resolved value is reused within one
	// process. Zero takes the default (5m).
	//
	// A cache rather than a lookup per call because one boot resolves
	// the same reference several times — the chat driver, the
	// capability probe and doctor all read the same provider key — and
	// on a CLI-backed vault each of those is a separate process.
	CacheTTL time.Duration `koanf:"cache_ttl,omitempty"`
}

// SecretProviderConfig is one declared secret backend.
type SecretProviderConfig struct {
	// Label is the reference scheme. "env" and "file" are refused:
	// they resolve against this machine before any provider can exist,
	// and letting one be shadowed would make the bootstrap path depend
	// on the thing it bootstraps.
	Label string `koanf:"label"`

	// Driver names the implementation: "exec" for any CLI, or
	// "bitwarden" / "onepassword" for the two whose failure modes are
	// worth translating.
	Driver string `koanf:"driver"`

	// Command overrides the driver's argv. Required by "exec" and
	// optional elsewhere, so a wrapper script that unlocks the vault
	// first does not need a new driver. "{{path}}" is replaced with the
	// reference's path; with no placeholder the path is appended.
	//
	// An argv, never a shell string — a secret path containing a space
	// must not be able to become a second command.
	Command []string `koanf:"command,omitempty"`

	// Env is extra environment for the subprocess, as plaintext. Most
	// of what a vault CLI needs is not secret — a config directory, a
	// CA bundle path, an account alias — and an earlier version routed
	// every value through the secret resolver, which made those
	// impossible to set at all.
	//
	// Split exactly as [mcp.servers.<name>] splits them, and for the
	// same reason: "Env pairs are plaintext; SecretEnv names env vars
	// whose values resolve via secret refs".
	Env map[string]string `koanf:"env,omitempty"`

	// SecretEnv names env vars whose values are secret references,
	// resolved through the BOOTSTRAP resolver only: a vault credential
	// cannot come from a vault.
	SecretEnv map[string]string `koanf:"secret_env,omitempty"`

	// Timeout bounds one fetch. Zero takes 15s, which is generous
	// because the alternative to waiting for a vault is a node that
	// will not boot.
	Timeout time.Duration `koanf:"timeout,omitempty"`

	// Options are driver-specific scalars, a map for the same reason
	// the search providers use one: a new backend should not require a
	// config-struct change.
	Options map[string]string `koanf:"options,omitempty"`
}

// DiscoveryConfig is the [discovery] section: how this node finds
// its peers. SeedNodes is the explicit list; Broadcast additionally
// enables LAN auto-discovery, which suits a home deployment but
// should stay off anywhere the broadcast domain is untrusted.
type DiscoveryConfig struct {
	SeedNodes         []string      `koanf:"seed_nodes"`
	Broadcast         bool          `koanf:"broadcast"`
	BroadcastPort     int           `koanf:"broadcast_port"`     // default 7445
	BroadcastAddress  string        `koanf:"broadcast_address"`  // default "255.255.255.255"
	BroadcastInterval time.Duration `koanf:"broadcast_interval"` // default 30s
}

// ClusterConfig is the [cluster] section: this node's identity, the
// addresses it listens on and advertises, where its state lives, and
// which functions it enables.
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

	// OperatorCACert and OperatorCAKey hold the root that signs
	// OPERATOR credentials. Separate from the cluster CA on purpose:
	// this key is online, and a key that can mint people must not be
	// one that can mint peers.
	//
	// Empty defaults to operator-ca.pem / operator-ca-key.pem beside
	// the cluster CA, created on first use. An operator CA is not
	// something anybody should have to configure before their first
	// enrolment.
	OperatorCACert string `koanf:"operator_ca_cert"`
	OperatorCAKey  string `koanf:"operator_ca_key"`

	// EnrolAddr is where a laptop with no credential yet submits a
	// certificate request. Empty disables enrolment entirely.
	//
	// Its own listener because it is the one surface that CANNOT
	// require a client certificate — the caller does not have one,
	// which is the whole point. Keeping it separate means the
	// cluster listener's "every caller presents a cert" is never
	// weakened to accommodate it.
	EnrolAddr string `koanf:"enrol_addr"`

	// EnrolValidFor is how long an issued operator certificate lasts.
	// Zero takes the 90-day default a travelling credential gets.
	EnrolValidFor time.Duration `koanf:"enrol_valid_for"`
}

// SoulLoaderConfig is the [soul] section, pointing at the SOUL.md
// that defines the agent's persona. Scope selects which portion of
// the file applies when one file serves several deployments.
type SoulLoaderConfig struct {
	Path string `koanf:"path"`
}

// AuthConfig is the [auth] section: JWT validation for inbound
// requests. Issuer and JWKSURL cover the normal asymmetric case;
// JWTSecretRef plus AllowHS256 exist for symmetric-signing setups
// and should stay off unless deliberately chosen.
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

// SandboxConfig is the [sandbox] section: the default Landlock,
// seccomp and network confinement applied to skills and
// shell_command. Storage-mount modes layer on top of these.
// SandboxConfig is the [sandbox] section.
//
// It holds ONE key. The sandbox is described by policy.d files, and
// this section used to declare a parallel vocabulary for the same
// thing — allowed_paths, read_only_paths, network_allow_cidr,
// dangerous_cmds_deny/allow, env_whitelist, cpu_quota,
// memory_limit_mb, skip_perm_checks, hot_reload_opt_out — none of
// which anything read. An operator setting network_allow_cidr was
// restricting nothing.
//
// They are gone rather than wired up, because two authorities for one
// sandbox is how they came to disagree in the first place.
type SandboxConfig struct {
	// PolicyDirs overrides the default policy.d discovery chain.
	// Leave empty in almost all cases — the loader derives a sensible
	// default (user-global → config-dir → cwd). When set, the caller
	// is explicit and the defaults are NOT merged in: this is the
	// "if I set --policy-dir, don't sneak in extras" behaviour.
	// Order matters: later dirs override earlier ones on same-tool
	// conflicts. A single string in the array is equivalent to the
	// old `policy_dir` key.
	PolicyDirs []string `koanf:"policy_dirs"`
}

// AuditConfig is the [audit] section. The two sinks are independent
// and may both be enabled: raft replicates the log cluster-wide,
// local keeps a rotated on-disk copy that survives losing quorum.
type AuditConfig struct {
	Raft  AuditRaftConfig  `koanf:"raft"`
	Local AuditLocalConfig `koanf:"local"`
}

// AuditRaftConfig is [audit.raft], the replicated audit sink.
//
// anchor_target / anchor_cadence used to sit here, described as
// publishing the hash-chain head so tampering is detectable. Nothing
// published anything. Removed rather than left as a claim the system
// does not make — see ROADMAP for the anchoring work itself.
type AuditRaftConfig struct {
	Enabled bool `koanf:"enabled"`
}

// AuditLocalConfig is [audit.local], the on-disk audit sink. Size
// and file counts bound the rotated set.
type AuditLocalConfig struct {
	Enabled   bool   `koanf:"enabled"`
	Path      string `koanf:"path"`
	MaxSizeMB int    `koanf:"max_size_mb"`
	MaxFiles  int    `koanf:"max_files"`
}

// SkillsConfig is the [skills] section: where skill manifests are
// loaded from and how their signatures are treated.
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

	// DevSource is a local directory whose skills outrank EVERYTHING,
	// including signed ones.
	//
	// The escape hatch for an operator who needs to override a signed
	// skill locally. It has to be a separate source rather than a way
	// to game precedence, because a rule that can be beaten by editing
	// a version number is not a rule.
	//
	// Gated twice: this key AND the LOBSLAW_DEV environment variable.
	// With the key set and the variable absent the node REFUSES TO
	// START, naming both. Either gate alone is easy to leave behind —
	// a config file gets copied to production wholesale, an
	// environment variable gets set in a shell profile and forgotten —
	// and both at once is a coincidence somebody has to arrange.
	//
	// Layout is <dir>/<name>/manifest.yaml, one level, because this is
	// a working directory somebody edits by hand.
	DevSource string `koanf:"dev_source,omitempty"`
}

// SecurityConfig carries cross-cutting safety controls: the egress
// filter's ACL inputs, future subprocess sandbox knobs, etc. Each
// field is independently optional — empty struct is valid and
// produces sensible-default behaviour (deny-by-default ACL with
// permissive fetch_url).
type SecurityConfig struct {
	// SessionGrantTTL bounds an "approve for the rest of this
	// conversation" grant. Zero takes the default of 24h.
	//
	// It exists because the previous bound was the process exiting.
	// That made the lifetime of a security grant a function of deploy
	// cadence — weeks on a stable cluster, ninety seconds during a
	// rollout — and neither of those is a decision anybody made.
	//
	// A day, by default, because the unit the user was reasoning about
	// is a conversation and conversations are a day-shaped thing.
	SessionGrantTTL time.Duration `koanf:"session_grant_ttl,omitempty"`

	// EgressUpstreamProxy is the corporate proxy lobslaw chains
	// through. Empty = direct egress. Format: "http://corp:8080"
	// or "https://...". Forwarded to smokescreen's
	// upstream-proxy hook.
	EgressUpstreamProxy string `koanf:"egress_upstream_proxy,omitempty"`

	// EgressAllowPrivateRanges disables smokescreen's RFC1918 deny.
	// NEVER set this in production — it lets a compromised process
	// reach the local network. Set only by dev/test setups that
	// need to talk to localhost-bound services (e.g. a self-hosted
	// LLM on the same machine).
	EgressAllowPrivateRanges bool `koanf:"egress_allow_private_ranges,omitempty"`

	// EgressAllowRanges is the explicit CIDR allowlist on top of
	// the default rules. Use to permit a single private subnet
	// (Tailscale tailnet, Wireguard mesh) without unlocking all
	// of RFC1918. Format: ["100.64.0.0/10", "10.0.0.0/24"].
	EgressAllowRanges []string `koanf:"egress_allow_ranges,omitempty"`

	// ClawhubBaseURL is the API endpoint for the clawhub.ai skill
	// catalog. Empty disables clawhub-driven install — operators
	// who want self-host or no clawhub access leave this off.
	ClawhubBaseURL string `koanf:"clawhub_base_url,omitempty"`

	// ClawhubBinaryHosts is the optional allowlist for skill-bundled
	// binary download URLs (Phase B). Default — when ClawhubBaseURL
	// is set — is github.com release hosts. Operators with stricter
	// supply-chain requirements declare their own.
	ClawhubBinaryHosts []string `koanf:"clawhub_binary_hosts,omitempty"`

	// ClawhubInstallMount names the storage mount where installed
	// skill bundles land. Empty = "skill-tools" (the canonical
	// label). Operators with custom layouts override.
	ClawhubInstallMount string `koanf:"clawhub_install_mount,omitempty"`

	// ClawhubAutoEmitInstallRules controls whether a successful
	// clawhub_install also writes a policy rule allowing the agent
	// to call the newly-installed skill (resource = <skill_name>,
	// subject = scope:owner, effect = allow, priority = 20). Default
	// false — operator must explicitly opt in. When true, the
	// emitted rule appears alongside operator-declared rules in the
	// policy bucket and survives reload. Operators who want skills
	// to require an explicit per-skill opt-in (e.g. for
	// require_confirmation on writes) leave this false.
	ClawhubAutoEmitInstallRules bool `koanf:"clawhub_auto_emit_install_rules,omitempty"`

	// EgressUDSPath, when set, makes smokescreen also listen on a
	// Unix-domain socket at the given path. Required when any skill
	// declares network_isolation: true (the netns subprocess can't
	// reach loopback TCP). Recommended location: under a directory
	// reachable from the subprocess's mount namespace + Landlock
	// allowlist (typically /tmp). Empty = TCP-only.
	EgressUDSPath string `koanf:"egress_uds_path,omitempty"`

	// StrictSkillEgress makes a skill that declares no `network:` be
	// DENIED egress rather than falling through to the default ACL.
	//
	// Opt-in because it is a breaking change for any skill written
	// before per-skill roles were populated: those manifests omit the
	// field, and every one of them stops reaching the network the day
	// this flips. The safe default leaves them on the default ACL and
	// warns at boot with their names.
	//
	// Turn it on once every skill on the node declares what it needs.
	// Boot warns with the names of those that do not, under either
	// setting, so the list to work through is in the log already.
	StrictSkillEgress bool `koanf:"strict_skill_egress,omitempty"`

	// FetchURLAllowHosts narrows the fetch_url builtin's egress.
	// Empty = permissive (any public host, smokescreen still blocks
	// private IPs). Non-empty = explicit allowlist; the agent's
	// fetch_url calls are limited to these hostnames.
	FetchURLAllowHosts []string `koanf:"fetch_url_allow_hosts,omitempty"`

	// BinaryInstallPrefix is the directory user-mode install managers
	// (npm/cargo/go-install/uvx/pipx/curl-sh) write to when satisfying
	// a clawhub skill's declared bin requirements. Defaults to
	// "/lobslaw/usr/local" (FHS-conventional for "operator-installed
	// locally", distinct from /lobslaw/usr/bin which the uv-init
	// container owns). System managers (apt/dnf/pacman/apk) ignore
	// this — they only land in deployments where the operator has
	// configured them and they write to system paths.
	BinaryInstallPrefix string `koanf:"binary_install_prefix,omitempty"`

	// OAuth declares the device-flow IdPs operators have registered
	// applications with. Keyed by provider name ("google", "github",
	// ...) which becomes the credentials-bucket prefix and the
	// egress role suffix ("oauth/<name>"). Empty = no oauth_start
	// flows can run; the builtins return an error pointing at the
	// missing config.
	OAuth map[string]OAuthProviderConfig `koanf:"oauth,omitempty"`
}

// OAuthProviderConfig is the operator-declared shape of one IdP
// registration. The package internal/oauth has well-known endpoint
// defaults for "google" and "github" — operators only need to fill
// ClientIDRef (and ClientSecretRef where the IdP requires one) for
// those. Custom providers declare endpoints explicitly.
type OAuthProviderConfig struct {
	// DeviceAuthEndpoint accepts the initial device-code request.
	// Leave empty for known providers ("google", "github") to use
	// the well-known default.
	DeviceAuthEndpoint string `koanf:"device_auth_endpoint,omitempty"`

	// TokenEndpoint exchanges device_code for tokens. Leave empty
	// for known providers.
	TokenEndpoint string `koanf:"token_endpoint,omitempty"`

	// ClientIDRef resolves to the OAuth app client_id via the
	// configured secret resolver (env var / file / vault).
	ClientIDRef string `koanf:"client_id_ref"`

	// ClientSecretRef is required by some IdPs even for device flow
	// (Google's "TVs and Limited Input Devices"); empty for public-
	// client providers like GitHub.
	ClientSecretRef string `koanf:"client_secret_ref,omitempty"`

	// DefaultScopes is the scope set requested when oauth_start is
	// invoked without explicit scopes. Operators override per-call.
	DefaultScopes []string `koanf:"default_scopes,omitempty"`

	// SubjectClaim is the response field that identifies the
	// authenticated user; used to derive the credentials bucket key.
	// Leave empty for known providers to inherit ("email" for
	// google, "login" for github).
	SubjectClaim string `koanf:"subject_claim,omitempty"`
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

// RemoteConfig is one host the agent may run development work on.
//
// The whole point of this block is that the AGENT CANNOT WRITE IT. A
// remote_ssh call names a remote; it does not supply a host, a port, a
// user or a key. Those are facts about the deployment, and the same
// reasoning applies as for compute.TurnIdentity: a value the model can
// choose is a request, not a fact, and the two must not share a channel.
// Without that split, "run this command over SSH" is a tool that dials
// anywhere, and the operator's only control is the prose in a prompt.
//
//	[[remote]]
//	name        = "go"
//	description = "Go toolchain, opencode, aide"
//	host        = "devbox-go.lobslaw-dev.svc.cluster.local"
//	port        = 2222
//	user        = "dev"
//	key_ref     = "file:/etc/lobslaw/remote/id_ed25519"
//	known_hosts = "/var/lobslaw/data/remote_known_hosts"
type RemoteConfig struct {
	// Name is what the agent passes to remote_ssh. Short and about
	// the stack ("go", "rust"), because it is what the model reasons
	// with and a hostname is not.
	Name string `koanf:"name"`

	// Description reaches the model in the tool schema, so it is how
	// the agent picks between remotes. Say what the toolchain is;
	// an empty one leaves the model guessing from the name.
	Description string `koanf:"description,omitempty"`

	Host string `koanf:"host"`
	// Port defaults to 2222 rather than 22: a devbox sshd running
	// unprivileged under a restricted PodSecurity policy cannot bind
	// a privileged port, so 2222 is the normal case here and 22 is
	// the exception.
	Port int    `koanf:"port,omitempty"`
	User string `koanf:"user,omitempty"`

	// KeyRef resolves to a PEM private key through the secrets
	// resolver — "file:/path" or "env:NAME". Prefer file: for a key:
	// an env var round-trips the newlines through whatever wrote it,
	// and the failure mode is an unparseable key at first dial rather
	// than at boot.
	KeyRef string `koanf:"key_ref"`

	// KeyPassphraseRef decrypts an encrypted private key. Empty means
	// the key is expected to be unencrypted, which is the normal shape
	// for a key a daemon uses unattended.
	KeyPassphraseRef string `koanf:"key_passphrase_ref,omitempty"`

	// KnownHosts is the path to an OpenSSH known_hosts file. It is a
	// PATH and not a secret ref because it is written as well as read:
	// an unknown host is recorded on first connect and pinned
	// thereafter.
	//
	// Trust-on-first-use, and deliberately not host-key verification
	// turned off. TOFU is weak exactly once — the first dial after the
	// file is created — and strong every time after. Disabled
	// verification is weak forever, and the difference matters here
	// because the agent's whole job on that connection is to run code.
	//
	// Empty disables persistence, NOT verification: the key is pinned
	// for the process lifetime and a change mid-run is still refused.
	KnownHosts string `koanf:"known_hosts,omitempty"`

	// DefaultTimeoutSecs bounds a call that does not ask for one.
	// Builds are slow, so this is minutes rather than the seconds
	// shell_command allows. Zero takes the built-in default.
	DefaultTimeoutSecs int `koanf:"default_timeout_secs,omitempty"`

	// MaxTimeoutSecs caps what a call may ask for. Zero takes the
	// built-in ceiling.
	MaxTimeoutSecs int `koanf:"max_timeout_secs,omitempty"`
}

// BinaryConfig is one operator-declared host binary. Mirrors the
// shape of a clawhub bundle's clawdbot.requires/install pair so the
// same binaries.Manager pool can satisfy either source. Operators
// declare binaries that aren't in clawhub (custom internal tooling,
// vendor CLIs, github-release artefacts) here.
type BinaryConfig struct {
	Name        string                `koanf:"name"`
	Description string                `koanf:"description,omitempty"`
	Detect      string                `koanf:"detect,omitempty"`
	Install     []BinaryInstallConfig `koanf:"install"`
	// Version is the desired version string. When non-empty AND
	// Detect is set, the auto-install at boot runs the detect
	// command, captures stdout, and looks for the Version substring.
	// If absent, treats this as a version mismatch and forces a
	// reinstall — i.e., the operator just bumped the URL to a newer
	// release and wants the binary to upgrade. Empty means "any
	// version is fine, only install when Detect/PATH says missing".
	Version string `koanf:"version,omitempty"`
	// HelpCommand runs after a successful install to capture the
	// binary's flag/usage info. Persisted to disk and surfaced to
	// the agent via binary_install's response so the agent learns
	// the available flags without manually running --help. Empty
	// defaults to "<name> --help"; set explicitly when the binary
	// uses an unconventional help flag (e.g. "ffmpeg -h").
	HelpCommand string `koanf:"help_command,omitempty"`
	// Env is a list of "KEY=VAL" pairs exported every time the
	// binary is invoked. lobslaw installs a /bin/sh shim at
	// <prefix>/bin/<name> that exports these then execs the real
	// binary at <prefix>/libexec/<name>. Use for HOME (so headless
	// tools find their own state dir), XDG_*, or tool-specific
	// account/profile selectors. Only applied when the binary
	// installs under <prefix>/bin (gh-release, curl-sh) — apt/brew
	// installs to system paths skip wrapping with a debug log.
	Env []string `koanf:"env,omitempty"`
	// PostInstall is free-form prose surfaced to the agent after a
	// successful install. Use for one-shot setup commands, env-var
	// hints, OAuth flow walkthroughs — anything the user expects the
	// agent to read and act on after the binary lands. Same shape as
	// a clawhub SKILL.md prose body.
	PostInstall string `koanf:"post_install,omitempty"`
}

// BinaryInstallConfig is one [[binary.install]] entry — a single
// candidate recipe for installing a binary. The OS, Arch and Distro
// fields are the match predicate (empty matches anything); Manager
// picks the installer and determines which of the remaining fields
// it consumes.
type BinaryInstallConfig struct {
	OS       string   `koanf:"os,omitempty"`
	Arch     string   `koanf:"arch,omitempty"`
	Distro   string   `koanf:"distro,omitempty"`
	Manager  string   `koanf:"manager"`
	Package  string   `koanf:"package,omitempty"`
	Repo     string   `koanf:"repo,omitempty"`
	URL      string   `koanf:"url,omitempty"`
	Checksum string   `koanf:"checksum,omitempty"`
	Sudo     bool     `koanf:"sudo,omitempty"`
	Args     []string `koanf:"args,omitempty"`
}
