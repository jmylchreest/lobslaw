package node

import (
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
)

const (
	taskApprovalRPCTimeout    time.Duration = 10 * time.Second
	nodeProposalTimeout       time.Duration = 5 * time.Second
	gatewayWriteDeadlineSlack time.Duration = 30 * time.Second
	defaultRaftJoinTimeout    time.Duration = 30 * time.Second
	joinCandidateWait         time.Duration = 2 * time.Second
	membershipPollInterval    time.Duration = 200 * time.Millisecond
	startupLeaderWait         time.Duration = 5 * time.Second
	nodeGracefulStopTimeout   time.Duration = 10 * time.Second
	egressStopTimeout         time.Duration = 2 * time.Second
	pprofReadHeaderTimeout    time.Duration = 5 * time.Second
	pprofShutdownTimeout      time.Duration = 2 * time.Second
	toolBootstrapTimeout      time.Duration = 5 * time.Minute
	toolVersionPreviewRunes   int           = 80
	embedderInitTimeout       time.Duration = 30 * time.Minute
	researchFindingImportance int32         = 7
	noticeChallengeLimit      int           = 5
	builtinAllowSeedPriority  int32         = 1
	builtinDenySeedPriority   int32         = 10
	userOwnerPrefix                         = "user:"
	// Allow framing overhead above the skill bundle's application limit.
	maxNodeGRPCMessageBytes int = 3 * memory.DefaultMaxSkillTotalBytes
)
