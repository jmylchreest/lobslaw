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
)

// SoulTuneRecordID is the constant key under BucketSoulTune. There
// is one tune record per cluster — the agent has one identity.
const SoulTuneRecordID = "soul:tune"

// allBuckets lists every bucket the store ensures exists on open.
var allBuckets = []string{
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
}
