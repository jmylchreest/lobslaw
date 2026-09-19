package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const (
	credentialRefreshTimeout = 30 * time.Second
	credentialPersistTimeout = 10 * time.Second
	credentialIssueTimeout   = time.Minute
)

// ErrCredentialRefreshUncertain forbids retrying a possibly consumed token.
// Reconnect the account to install a new credential generation.
var ErrCredentialRefreshUncertain = errors.New("credentials: refresh outcome uncertain; reconnect the account")
var ErrCredentialChanged = errors.New("credentials: account changed during refresh; retry with current account")

type credentialRefreshResult struct {
	record *lobslawv1.CredentialRecord
	err    error
}

func (s *CredentialService) loadCredential(provider, subject string) (*lobslawv1.CredentialRecord, error) {
	if s.store == nil {
		return nil, errors.New("credentials: store not wired")
	}
	key, err := CredentialKey(provider, subject)
	if err != nil {
		return nil, err
	}
	raw, err := s.store.Get(BucketCredentials, key)
	if err != nil {
		return nil, err
	}
	var rec lobslawv1.CredentialRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (s *CredentialService) applyCredentialEntry(ctx context.Context, data []byte) error {
	if s.raft == nil {
		return errors.New("credentials: raft not wired")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	response, err := s.raft.ApplyOrForward(ctx, data, credentialApplyTimeout)
	if err != nil {
		return err
	}
	if failure, ok := response.(error); ok {
		return failure
	}
	return nil
}

func (s *CredentialService) claimCredential(ctx context.Context, before, after *lobslawv1.CredentialRecord) error {
	key, err := CredentialKey(before.Provider, before.Subject)
	if err != nil {
		return err
	}
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: key, ExpectedRevision: &before.Revision, ExpectedClaimer: before.ClaimedBy,
		Payload: &lobslawv1.LogEntry_Credential{Credential: after},
	})
	if err != nil {
		return err
	}
	return s.applyCredentialEntry(ctx, data)
}

func waitCredential(ctx context.Context) error {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *CredentialService) editCredentialACL(ctx context.Context, provider, subject string, edit func(*lobslawv1.CredentialRecord) error) error {
	ctx, cancel := context.WithTimeout(ctx, credentialPersistTimeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		before, err := s.loadCredential(provider, subject)
		if err != nil {
			return err
		}
		after := proto.Clone(before).(*lobslawv1.CredentialRecord)
		if err := edit(after); err != nil {
			return err
		}
		if err := s.claimCredential(ctx, before, after); !errors.Is(err, ErrClaimConflict) {
			return err
		}
		if err := waitCredential(ctx); err != nil {
			return err
		}
	}
}

// Permissions are checked for each caller, including after a shared refresh.
// Provider scope reductions cannot leave an old ACL advertising removed scopes.
func (s *CredentialService) credentialIssue(rec *lobslawv1.CredentialRecord, skill string) (*SkillIssue, error) {
	p, err := s.decrypt(rec)
	if err != nil {
		return nil, err
	}
	var scopes []string
	for _, scope := range s.ScopesAllowedForSkill(p, skill) {
		if slices.Contains(p.Scopes, scope) {
			scopes = append(scopes, scope)
		}
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("credentials: skill %q is not authorised for %s/%s (run credentials_grant first)", skill, p.Provider, p.Subject)
	}
	return &SkillIssue{AccessToken: p.AccessToken, Scopes: scopes, ExpiresAt: p.ExpiresAt}, nil
}

func (s *CredentialService) refreshCredential(parent context.Context, claimed *lobslawv1.CredentialRecord, refresher TokenRefresher) credentialRefreshResult {
	ctx, cancel := context.WithTimeout(parent, credentialRefreshTimeout)
	defer cancel()
	p, err := s.decrypt(claimed)
	if err != nil {
		return credentialRefreshResult{err: err}
	}
	access, refresh, expiresIn, scope, err := refresher(ctx, p.RefreshToken)
	if err != nil || access == "" {
		// A network error may hide a successful rotation. Never release ownership
		// for another attempt with the old token, even if this process restarts.
		_, _ = s.finishCredentialRefresh(parent, claimed, nil)
		return credentialRefreshResult{err: ErrCredentialRefreshUncertain}
	}
	now := time.Now()
	p.AccessToken = access
	if refresh != "" {
		p.RefreshToken = refresh
	}
	if expiresIn > 0 {
		p.ExpiresAt = now.Add(time.Duration(expiresIn) * time.Second)
	}
	if scope != "" {
		p.Scopes = strings.Fields(strings.ReplaceAll(scope, ",", " "))
	}
	p.LastRotated = now
	tokens, err := s.encrypt(p)
	if err != nil {
		return credentialRefreshResult{err: ErrCredentialRefreshUncertain}
	}
	rec, err := s.finishCredentialRefresh(parent, claimed, tokens)
	if err != nil {
		return credentialRefreshResult{err: err}
	}
	return credentialRefreshResult{record: rec}
}

// A successful rotation may race an ACL edit. Rebuild only token fields on the
// latest record and CAS again; never re-send the provider request.
// nil tokens persist the uncertain state while retaining attempt ownership.
func (s *CredentialService) finishCredentialRefresh(parent context.Context, claimed, tokens *lobslawv1.CredentialRecord) (*lobslawv1.CredentialRecord, error) {
	ctx, cancel := context.WithTimeout(parent, credentialPersistTimeout)
	defer cancel()
	before := claimed
	for {
		if before.Generation != claimed.Generation {
			return nil, ErrCredentialChanged
		}
		if before.Revision >= claimed.Revision {
			if before.ClaimedBy != claimed.ClaimedBy {
				// The previous CAS may have committed despite a lost response.
				if tokens != nil && before.ClaimedBy == "" && bytes.Equal(before.AccessToken, tokens.AccessToken) && bytes.Equal(before.RefreshToken, tokens.RefreshToken) {
					return before, nil
				}
				return nil, ErrCredentialChanged
			}
			after := proto.Clone(before).(*lobslawv1.CredentialRecord)
			after.RefreshUncertain = tokens == nil
			if tokens != nil {
				after.AccessToken = tokens.AccessToken
				after.RefreshToken = tokens.RefreshToken
				after.ExpiresAt = tokens.ExpiresAt
				after.LastRotated = tokens.LastRotated
				after.Scopes = tokens.Scopes
				after.ClaimedBy = ""
				after.RefreshDeadline = nil
			}
			if err := s.claimCredential(ctx, before, after); err == nil {
				after.Revision = before.Revision + 1
				return after, nil
			}
		}
		if err := waitCredential(ctx); err != nil {
			return nil, ErrCredentialRefreshUncertain
		}
		current, err := s.loadCredential(claimed.Provider, claimed.Subject)
		if err != nil {
			if IsNotFound(err) {
				return nil, ErrCredentialChanged
			}
			// Preserve the rotated pair in memory while retrying transient reads.
			continue
		}
		before = current
	}
}
