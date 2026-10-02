package memory

import "time"

// Package policy defaults, resource limits, and protocol bounds.
const (
	maxShareBatchBytes            int           = 4 << 20
	storeOpenTimeout              time.Duration = 5 * time.Second
	MaxTaskApprovalListLimit      int32         = 100
	DefaultTaskApprovalListLimit  int32         = 50
	maxTaskIdentityBytes          int           = 256
	maxTaskResultBytes            int           = 64 << 10
	DefaultImportance             int32         = 5
	MinImportance                 int32         = 1
	MaxImportance                 int32         = 10
	retainedSnapshots             int           = 2
	DefaultRaftHeartbeatTimeout   time.Duration = 500 * time.Millisecond
	DefaultRaftElectionTimeout    time.Duration = 500 * time.Millisecond
	DefaultRaftLeaderLeaseTimeout time.Duration = 250 * time.Millisecond
	DefaultRaftCommitTimeout      time.Duration = 50 * time.Millisecond
	leaderPollInterval            time.Duration = 200 * time.Millisecond
	stateLogInterval              time.Duration = 30 * time.Second
	stateReconcileInterval        time.Duration = 1 * time.Second
	addVoterTimeout               time.Duration = 10 * time.Second
	raftShutdownPollInterval      time.Duration = 10 * time.Millisecond
	raftObservationBuffer         int           = 16
	DefaultDreamMaxCandidates     int           = 10
	DefaultDreamMaxMergeClusters  int           = 10
	DefaultDreamPruneThreshold    float32       = 0.1
	DefaultDreamHalfLife          time.Duration = 14 * 24 * time.Hour
	DefaultDreamCommitmentGrace   time.Duration = 24 * time.Hour
	dreamSessionImportance        int32         = 3
	reminderImportance            int32         = 4
	maxReminderLabelRunes         int           = 80
	DefaultSearchLimit            int           = 10
	DefaultSessionMaxAge          time.Duration = 24 * time.Hour
	DefaultPromptPollInterval     time.Duration = 250 * time.Millisecond
	credentialRefreshPollInterval time.Duration = 20 * time.Millisecond
	similarityNameWeight          float64       = 0.7
	similarityDescriptionWeight   float64       = 0.3
	maxDisplayedSimilarArtefacts  int           = 3
	duplicateCandidateLimit       int           = 5
	taskApprovalApplyTimeout      time.Duration = 5 * time.Second
	skillApplyTimeout             time.Duration = 5 * time.Second
	shareApplyTimeout             time.Duration = 5 * time.Second
	sessionGrantApplyTimeout      time.Duration = 5 * time.Second
	selfTaughtApplyTimeout        time.Duration = 5 * time.Second
	reembedApplyTimeout           time.Duration = 5 * time.Second
	rebindApplyTimeout            time.Duration = 5 * time.Second
	promptApplyTimeout            time.Duration = 5 * time.Second
	pinnedApplyTimeout            time.Duration = 5 * time.Second
	enrolmentApplyTimeout         time.Duration = 5 * time.Second
	DefaultEpisodicApplyTimeout   time.Duration = 5 * time.Second
	archiveApplyTimeout           time.Duration = 30 * time.Second
)

// Packed protobuf float values occupy four bytes on the wire.
const protobufFloat32Bytes int = 4
