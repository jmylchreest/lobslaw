package memory

// Bucket names inside state.db. Each record type lives in its own
// top-level bbolt bucket, keyed by record ID.
const (
	BucketPolicyRules     = "policy_rules"
	BucketScheduledTasks  = "scheduled_tasks"
	BucketCommitments     = "commitments"
	BucketAuditEntries    = "audit_entries"
	BucketVectorRecords   = "vector_records"
	BucketEpisodicRecords = "episodic_records"
	BucketStorageMounts   = "storage_mounts"
	// BucketChannelState holds per-channel resume state for gateway
	// channels (telegram update offset, REST cursors, webhook
	// last-seen timestamps). Keyed by "<channel>:<key>" — single
	// bucket avoids per-channel bucket proliferation while keeping
	// scans cheap and predictable.
	BucketChannelState = "channel_state"
	// BucketSoulTune holds the cluster-wide agent personality overlay
	// — name, emotive dimensions, fragments. Single record keyed by
	// SoulTuneRecordID. Replaces the local SOUL.md write path so
	// container deployments don't need a writable file mount.
	BucketSoulTune = "soul_tune"
	// BucketCredentials holds OAuth (and other) credentials the
	// operator has connected to the cluster. Tokens are encrypted
	// at rest with the cluster MemoryKey; the bucket bytes are
	// ciphertext. Keyed by "<provider>:<subject>" — one record per
	// (provider, authenticated-user) tuple.
	BucketCredentials = "credentials"
	// BucketSessions holds the per-conversation index record: which
	// channel + user, the retained sequence range, the title. Keyed
	// by "<channel>:<channel_id>". One record per live conversation.
	BucketSessions = "sessions"
	// BucketSessionMessages holds the transcript bodies, keyed
	// "<session_id>:<20-digit zero-padded seq>". The padding makes
	// bbolt's byte ordering identical to sequence ordering, so a
	// session's thread is an ordered prefix scan and trimming is a
	// delete of the lowest keys.
	BucketSessionMessages = "session_messages"

	// BucketRaftMeta holds the FSM's own bookkeeping rather than any
	// replicated record — currently just the last raft index applied.
	//
	// It exists because this FSM's state is DURABLE. hashicorp/raft
	// assumes an FSM is rebuilt by replaying the log (in-memory) or by
	// Restore-then-replay; ours is a bbolt file that already holds the
	// final state, so a replay from index 1 re-applies history on top
	// of it. Recording how far we got is what makes replay idempotent.
	BucketRaftMeta = "raft_meta"

	// KeyLastAppliedIndex is the only key in BucketRaftMeta.
	KeyLastAppliedIndex = "last_applied_index"
	// BucketUserPrefs holds per-user preferences: timezone,
	// subscribed channel addresses (telegram chat_id, future Slack
	// user, etc.), language. Keyed by canonical user_id. Plaintext
	// — channel IDs aren't secret, timezones aren't secret.
	// Solo-deployment uses one record (id=owner); team/corporate
	// deployments scale by adding records.
	BucketUserPrefs = "user_prefs"

	// BucketSessionLeases holds cluster-wide turn ownership, one
	// record per conversation. Separate from BucketSessions because a
	// lease is written three times per turn (claim, heartbeats,
	// release) and the transcript is written once — sharing a record
	// would make every lease write contend with the append made by
	// the turn holding it.
	BucketSessionLeases = "session_leases"

	// BucketPrompts holds pending confirmations. Raft-backed rather
	// than per-process so an approval tapped on one node resolves a
	// prompt issued by another, and so a restart does not lose the
	// turn the user was answering. See R2.
	BucketPrompts = "prompts"

	// BucketConsolidations holds Dream's adjudication log: what it
	// decided about each cluster of near-duplicate memories and why.
	// Read by `lobslaw memory consolidations`; pruned by Dream itself
	// so a long-lived cluster does not accumulate a record per cluster
	// per night forever.
	BucketConsolidations = "consolidations"

	// BucketDisputes indexes records Dream found in disagreement, so
	// recall can ask "is this hit disputed" without scanning the
	// consolidation log on every turn. Keyed
	// "<episodic id>/<consolidation id>", valued with the
	// consolidation id — the verdict itself stays in one place, and
	// this is only the way back to it.
	//
	// Derived state that is still replicated: it is written by the
	// same entry that writes the verdict, so a follower cannot end up
	// holding one without the other.
	BucketDisputes = "disputes"

	// BucketPinned holds the always-on memory blocks rendered into
	// every system prompt: the user profile and the agent's notes.
	// Small and capped by design — it is a fixed tax on every request.
	BucketPinned = "pinned"

	// The self-taught store. Provenance by location: if a record is
	// here, the agent authored it — there is no marker to forget,
	// forge, or lose.
	//
	// Three buckets rather than a state field alone, because "show me
	// everything it decided on its own" and "show me what it has
	// stopped using" are different questions asked at different times,
	// and the archive must be a place things move TO rather than a
	// filter over the live set.
	BucketSelfTaught        = "self_taught"
	BucketSelfTaughtUsage   = "self_taught_usage"
	BucketSelfTaughtArchive = "self_taught_archive"

	// BucketSelfTaughtHistory holds prior versions, keyed
	// "<id>@<zero-padded version>" so a prefix scan is version order.
	// Bounded by history_depth: every version lives in every snapshot
	// on every node, so unbounded history is a store-growth problem
	// that only shows up months later as slow snapshots.
	BucketSelfTaughtHistory = "self_taught_history"

	// BucketSessionGrants holds "approved for the rest of this
	// conversation" answers, keyed
	// "<session_id>\x00<action>\x00<resource>" so a conversation's
	// grants are a prefix scan and dropping a conversation drops them.
	//
	// Replicated rather than per-process. The in-process version was
	// defensible against restarts and never was against a cluster: the
	// user answered in one conversation and got asked again because
	// the next message landed on a different node.
	BucketSessionGrants = "session_grants"

	// BucketSkills holds imported skills — the operator and signed
	// tiers — keyed "<name>@<version>".
	//
	// Authority moves here from the filesystem. A skill exists because
	// the log says so rather than because a mount happens to be
	// materialised on the node you asked, which is what made "is this
	// skill installed?" a question with a per-node answer.
	BucketSkills = "skills"

	// BucketSkillBlobs holds content-addressed payloads — handlers,
	// reference documents — keyed by "sha256:<hex>".
	//
	// Separate from the record so two versions of a skill sharing a
	// reference store it once, and so listing skills does not pull
	// every payload they mention.
	BucketSkillBlobs = "skill_blobs"

	// BucketEnrolments holds operator enrolment requests.
	//
	// Replicated rather than in-memory, unlike the confirmation
	// registry: a request that vanished on restart would leave a
	// laptop polling an id no node has heard of, with no way to tell
	// that from "still waiting". It also lets the node that ANSWERS an
	// enrolment be a different one from the node that received it.
	BucketEnrolments = "enrolments"
)

// SoulTuneRecordID is the constant key under BucketSoulTune. There
// is one tune record per cluster — the agent has one identity.
const SoulTuneRecordID = "soul:tune"

// allBuckets lists every bucket the store ensures exists on open.
var allBuckets = []string{
	BucketRaftMeta,
	BucketPolicyRules,
	BucketScheduledTasks,
	BucketCommitments,
	BucketAuditEntries,
	BucketVectorRecords,
	BucketEpisodicRecords,
	BucketStorageMounts,
	BucketChannelState,
	BucketSoulTune,
	BucketCredentials,
	BucketUserPrefs,
	BucketSessions,
	BucketSessionMessages,
	BucketSessionLeases,
	BucketPrompts,
	BucketConsolidations,
	BucketDisputes,
	BucketPinned,
	BucketSelfTaught,
	BucketSelfTaughtUsage,
	BucketSelfTaughtArchive,
	BucketSessionGrants,
	BucketSkills,
	BucketSkillBlobs,
	BucketSelfTaughtHistory,
	BucketEnrolments,
}
