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
)

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
}
