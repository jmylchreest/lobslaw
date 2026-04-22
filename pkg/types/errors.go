package types

import "errors"

// Sentinel errors. Wrap with fmt.Errorf("context: %w", err) when
// returning from functions; check with errors.Is at call sites.
var (
	ErrNotFound               = errors.New("not found")
	ErrDenied                 = errors.New("denied")
	ErrConfirmationRequired   = errors.New("confirmation required")
	ErrConfirmationTimeout    = errors.New("confirmation timeout")
	ErrClaimExpired           = errors.New("claim expired")
	ErrAlreadyClaimed         = errors.New("already claimed by another node")
	ErrNoProvider             = errors.New("no provider meets required trust tier")
	ErrBudgetExceeded         = errors.New("turn budget exceeded")
	ErrInvalidConfig          = errors.New("invalid config")
	ErrMissingSecret          = errors.New("missing secret reference")
	ErrUnknownSecretScheme    = errors.New("unknown secret-ref scheme")
	ErrInvalidToolArgs        = errors.New("invalid tool arguments")
	ErrPathOutsideSandbox     = errors.New("path outside sandbox allowed_paths")
	ErrSandboxEscapeAttempt   = errors.New("sandbox escape attempt")
	ErrPolicyBypass           = errors.New("policy bypass attempted")
	ErrSkillUntrusted         = errors.New("skill not approved by operator")
	ErrSkillSHAMismatch       = errors.New("skill tree SHA differs from approved value")
	ErrSkillSignatureInvalid  = errors.New("skill signature invalid")
	ErrAuditChainBroken       = errors.New("audit chain verification failed")
	ErrRaftNotLeader          = errors.New("raft: not the leader")
	ErrRaftNoLeader           = errors.New("raft: no leader elected")
	ErrHookBlocked            = errors.New("hook returned block decision")
	ErrHookTimeout            = errors.New("hook timed out")
)
