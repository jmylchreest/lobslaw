package memory

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// credentialApplyTimeout caps raft.Apply for credential writes.
// OAuth refreshes happen at human-pace cadence; 5s is generous.
const credentialApplyTimeout = 5 * time.Second

// CredentialService manages the encrypted credential bucket.
// Tokens (access + refresh) are encrypted with the cluster
// MemoryKey before they ever land in the bucket; the proto bytes
// on disk and over the wire are ciphertext.
//
// Reads are local. Writes go through Raft so credentials replicate
// to every cluster node (any node that handles a skill invocation
// must be able to issue tokens to the subprocess).
//
// ACL fields (AllowedSkills, AllowedScopesPerSkill) are populated
// ONLY by operator commands — never by the agent. New credentials
// from oauth_start arrive with empty ACL and are inert until the
// operator explicitly grants per-skill access.
type CredentialService struct {
	raft  *RaftNode
	store *Store
	key   crypto.Key
}

// NewCredentialService wires the service. The MemoryKey is the
// same cluster-wide encryption key that secures state.db at rest
// — reusing it means there's one master secret to manage, not two.
//
// Nil raft → writes return an error; reads still work locally.
// Zero-key → returns an error (encrypted records would be
// unreadable on every later boot).
func NewCredentialService(raft *RaftNode, store *Store, key crypto.Key) (*CredentialService, error) {
	if (key == crypto.Key{}) {
		return nil, errors.New("credentials: MemoryKey required for token encryption")
	}
	return &CredentialService{raft: raft, store: store, key: key}, nil
}

// CredentialKey is the bucket key for a (provider, subject) tuple.
// Format: "<provider>:<subject>". Both fields must be non-empty
// and free of ":". Subject is typically the authenticated user's
// email or login (e.g. "user@example.com" for Google).
func CredentialKey(provider, subject string) (string, error) {
	provider = strings.TrimSpace(provider)
	subject = strings.TrimSpace(subject)
	if provider == "" {
		return "", errors.New("credentials: provider required")
	}
	if subject == "" {
		return "", errors.New("credentials: subject required")
	}
	if strings.Contains(provider, ":") || strings.Contains(subject, ":") {
		return "", errors.New("credentials: provider/subject must not contain ':'")
	}
	return provider + ":" + subject, nil
}

// PlaintextCredential is the decrypted form callers work with.
// Returned by Get/Issue calls; never persisted in this shape.
// AccessToken / RefreshToken are decrypted bytes.
type PlaintextCredential struct {
	ID                    string
	Provider              string
	Subject               string
	AccessToken           string
	RefreshToken          string
	Scopes                []string
	ExpiresAt             time.Time
	CreatedAt             time.Time
	LastRotated           time.Time
	LastUsed              time.Time
	AllowedSkills         []string
	AllowedScopesPerSkill map[string][]string
}

// Get returns a decrypted credential by (provider, subject). Returns
// types.ErrNotFound when no record exists. Reads are local; no
// raft round-trip.
func (s *CredentialService) Get(_ context.Context, provider, subject string) (*PlaintextCredential, error) {
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
		return nil, fmt.Errorf("credentials: unmarshal %s: %w", key, err)
	}
	return s.decrypt(&rec)
}

// List returns every credential in the bucket, decrypted. Used by
// the operator's "credentials list" CLI/builtin. Sensitive — caller
// must apply the appropriate authorization gate (scope:owner only).
func (s *CredentialService) List(_ context.Context) ([]*PlaintextCredential, error) {
	if s.store == nil {
		return nil, errors.New("credentials: store not wired")
	}
	var out []*PlaintextCredential
	err := s.store.ForEach(BucketCredentials, func(_ string, raw []byte) error {
		var rec lobslawv1.CredentialRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return err
		}
		decoded, derr := s.decrypt(&rec)
		if derr != nil {
			return derr
		}
		out = append(out, decoded)
		return nil
	})
	return out, err
}

// Put writes a credential. Encrypts tokens before raft.Apply.
// Explicit account replacement, forwarded to the leader as necessary. A new
// generation invalidates any refresh in flight; Grant/Revoke and refresh use CAS.
func (s *CredentialService) Put(ctx context.Context, p *PlaintextCredential) error {
	if p == nil {
		return errors.New("credentials: nil credential")
	}
	if s.raft == nil {
		return errors.New("credentials: raft not wired")
	}
	key, err := CredentialKey(p.Provider, p.Subject)
	if err != nil {
		return err
	}
	rec, err := s.encrypt(p)
	if err != nil {
		return err
	}
	rec.Generation = ids.New()
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      key,
		Payload: &lobslawv1.LogEntry_Credential{Credential: rec},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("credentials: marshal: %w", err)
	}
	if err := s.applyCredentialEntry(ctx, data); err != nil {
		return fmt.Errorf("credentials: raft apply: %w", err)
	}
	return nil
}

// Delete removes a credential by (provider, subject). Leader-only.
// Used by the "credentials revoke" CLI/builtin.
func (s *CredentialService) Delete(ctx context.Context, provider, subject string) error {
	if s.raft == nil {
		return errors.New("credentials: raft not wired")
	}
	key, err := CredentialKey(provider, subject)
	if err != nil {
		return err
	}
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_DELETE,
		Id: key,
		Payload: &lobslawv1.LogEntry_Credential{
			Credential: &lobslawv1.CredentialRecord{Provider: provider, Subject: subject},
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("credentials: marshal: %w", err)
	}
	if err := s.applyCredentialEntry(ctx, data); err != nil {
		return fmt.Errorf("credentials: raft apply: %w", err)
	}
	return nil
}

// Grant adds a skill to the credential's AllowedSkills + sets its
// scope subset. Idempotent — re-granting overwrites the prior
// scope list. Empty scopes ⇒ skill loses access (scope subset of
// nothing equals nothing); use Revoke to remove the skill from
// AllowedSkills entirely.
func (s *CredentialService) Grant(ctx context.Context, provider, subject, skill string, scopes []string) error {
	return s.editCredentialACL(ctx, provider, subject, func(rec *lobslawv1.CredentialRecord) error {
		for _, scope := range scopes {
			if !slices.Contains(rec.Scopes, scope) {
				return fmt.Errorf("credentials: cannot grant scope %q — not in credential scopes", scope)
			}
		}
		if !slices.Contains(rec.AllowedSkills, skill) {
			rec.AllowedSkills = append(rec.AllowedSkills, skill)
		}
		if rec.AllowedScopesPerSkill == nil {
			rec.AllowedScopesPerSkill = make(map[string]*lobslawv1.AllowedScopes)
		}
		rec.AllowedScopesPerSkill[skill] = &lobslawv1.AllowedScopes{Scopes: append([]string(nil), scopes...)}
		return nil
	})
}

// Revoke removes a skill from the credential's ACL. The credential
// itself stays — Revoke only narrows access. Use Delete to remove
// the credential entirely.
func (s *CredentialService) Revoke(ctx context.Context, provider, subject, skill string) error {
	return s.editCredentialACL(ctx, provider, subject, func(rec *lobslawv1.CredentialRecord) error {
		rec.AllowedSkills = removeString(rec.AllowedSkills, skill)
		delete(rec.AllowedScopesPerSkill, skill)
		return nil
	})
}

// ScopesAllowedForSkill returns the scope subset a given skill may
// request from this credential. Empty slice when the skill isn't
// in AllowedSkills. Used by the credentials_request builtin to
// validate per-skill scope subsetting.
func (s *CredentialService) ScopesAllowedForSkill(p *PlaintextCredential, skill string) []string {
	if p == nil || !slices.Contains(p.AllowedSkills, skill) {
		return nil
	}
	return p.AllowedScopesPerSkill[skill]
}

// FindOnlyForProvider returns the single credential bound to the
// given provider. Errors when zero or multiple are stored — callers
// in single-user setups can treat "the google credential" as
// implicit; multi-user setups MUST disambiguate by subject. Reads
// are local; no raft round-trip.
func (s *CredentialService) FindOnlyForProvider(ctx context.Context, provider string) (*PlaintextCredential, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	var matches []*PlaintextCredential
	for _, c := range all {
		if c.Provider == provider {
			matches = append(matches, c)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("credentials: no credential bound for provider %q", provider)
	case 1:
		return matches[0], nil
	default:
		subjects := make([]string, 0, len(matches))
		for _, m := range matches {
			subjects = append(subjects, m.Subject)
		}
		return nil, fmt.Errorf("credentials: provider %q has %d bound credentials (%v); subject must be specified", provider, len(matches), subjects)
	}
}

// TokenRefresher trades a refresh token for a fresh access token.
// Decoupled from the oauth package so memory can stay independent of
// the device-flow specifics — node wiring injects oauth.RefreshToken
// closed over the resolved ProviderConfig.
type TokenRefresher func(ctx context.Context, refreshToken string) (access string, refresh string, expiresIn int, scope string, err error)

// SkillIssue is the result of IssueForSkill: a fresh access token
// scoped to what the skill is allowed to request, plus expiry info
// the invoker uses to decide what env vars to inject.
type SkillIssue struct {
	AccessToken string
	Scopes      []string
	ExpiresAt   time.Time
}

// refreshSkew is the buffer subtracted from ExpiresAt when deciding
// whether to refresh. Tokens that expire within this window are
// proactively refreshed so a long-running skill doesn't hit a 401
// mid-execution.
const refreshSkew = 60 * time.Second

// IssueForSkill returns a fresh access token for (provider, subject)
// that is authorised to act on behalf of the named skill. Validates
// the per-skill ACL, refreshes the token when within the skew window
// of expiry, persists the new token via raft so other nodes see the
// rotation, and returns the result.
//
// Refresh is optional — when refresher is nil the function returns
// the stored access token even if expired (caller decides whether
// that's fatal). Callers in production wire oauth.RefreshToken so
// rotation happens transparently.
func (s *CredentialService) IssueForSkill(ctx context.Context, provider, subject, skill string, refresher TokenRefresher) (*SkillIssue, error) {
	ctx, cancel := context.WithTimeout(ctx, credentialIssueTimeout)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec, err := s.loadCredential(provider, subject)
		if err != nil {
			return nil, err
		}
		issue, err := s.credentialIssue(rec, skill)
		if err != nil {
			return nil, err
		}
		switch {
		case rec.ClaimedBy != "":
			if rec.RefreshUncertain || rec.RefreshDeadline == nil || !rec.RefreshDeadline.AsTime().After(time.Now()) {
				return nil, ErrCredentialRefreshUncertain
			}
		case refresher == nil || rec.ExpiresAt == nil || time.Until(rec.ExpiresAt.AsTime()) >= refreshSkew:
			return issue, nil
		default:
			claimed := proto.Clone(rec).(*lobslawv1.CredentialRecord)
			claimed.ClaimedBy = ids.New()
			claimed.RefreshDeadline = timestamppb.New(time.Now().Add(credentialRefreshTimeout + credentialPersistTimeout))
			if err := s.claimCredential(ctx, rec, claimed); err != nil {
				if !errors.Is(err, ErrClaimConflict) {
					return nil, err
				}
			} else {
				claimed.Revision = rec.Revision + 1
				// A caller leaving must not discard the provider's rotated token.
				done := make(chan credentialRefreshResult, 1)
				go func() { done <- s.refreshCredential(context.WithoutCancel(ctx), claimed, refresher) }()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case result := <-done:
					if result.err != nil {
						return nil, result.err
					}
					return s.credentialIssue(result.record, skill)
				}
			}
		}
		if err := waitCredential(ctx); err != nil {
			return nil, err
		}
	}
}

// encrypt seals AccessToken + RefreshToken with the cluster key.
// Other fields stay plaintext — they're not secrets, and keeping
// them readable lets operators inspect the bucket via standard
// raft introspection without round-tripping through this service.
func (s *CredentialService) encrypt(p *PlaintextCredential) (*lobslawv1.CredentialRecord, error) {
	access, err := crypto.Seal(s.key, []byte(p.AccessToken))
	if err != nil {
		return nil, fmt.Errorf("credentials: seal access token: %w", err)
	}
	refresh, err := crypto.Seal(s.key, []byte(p.RefreshToken))
	if err != nil {
		return nil, fmt.Errorf("credentials: seal refresh token: %w", err)
	}
	rec := &lobslawv1.CredentialRecord{
		Id:            p.ID,
		Provider:      p.Provider,
		Subject:       p.Subject,
		AccessToken:   access,
		RefreshToken:  refresh,
		Scopes:        p.Scopes,
		AllowedSkills: p.AllowedSkills,
	}
	if !p.ExpiresAt.IsZero() {
		rec.ExpiresAt = timestamppb.New(p.ExpiresAt)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	rec.CreatedAt = timestamppb.New(p.CreatedAt)
	if !p.LastRotated.IsZero() {
		rec.LastRotated = timestamppb.New(p.LastRotated)
	}
	if !p.LastUsed.IsZero() {
		rec.LastUsed = timestamppb.New(p.LastUsed)
	}
	if len(p.AllowedScopesPerSkill) > 0 {
		rec.AllowedScopesPerSkill = make(map[string]*lobslawv1.AllowedScopes, len(p.AllowedScopesPerSkill))
		for skill, scopes := range p.AllowedScopesPerSkill {
			rec.AllowedScopesPerSkill[skill] = &lobslawv1.AllowedScopes{Scopes: append([]string(nil), scopes...)}
		}
	}
	return rec, nil
}

func (s *CredentialService) decrypt(rec *lobslawv1.CredentialRecord) (*PlaintextCredential, error) {
	access, err := crypto.Open(s.key, rec.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("credentials: open access token: %w", err)
	}
	refresh, err := crypto.Open(s.key, rec.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("credentials: open refresh token: %w", err)
	}
	out := &PlaintextCredential{
		ID:            rec.Id,
		Provider:      rec.Provider,
		Subject:       rec.Subject,
		AccessToken:   string(access),
		RefreshToken:  string(refresh),
		Scopes:        append([]string(nil), rec.Scopes...),
		AllowedSkills: append([]string(nil), rec.AllowedSkills...),
	}
	if rec.ExpiresAt != nil {
		out.ExpiresAt = rec.ExpiresAt.AsTime()
	}
	if rec.CreatedAt != nil {
		out.CreatedAt = rec.CreatedAt.AsTime()
	}
	if rec.LastRotated != nil {
		out.LastRotated = rec.LastRotated.AsTime()
	}
	if rec.LastUsed != nil {
		out.LastUsed = rec.LastUsed.AsTime()
	}
	if len(rec.AllowedScopesPerSkill) > 0 {
		out.AllowedScopesPerSkill = make(map[string][]string, len(rec.AllowedScopesPerSkill))
		for skill, scopes := range rec.AllowedScopesPerSkill {
			if scopes == nil {
				continue
			}
			out.AllowedScopesPerSkill[skill] = append([]string(nil), scopes.Scopes...)
		}
	}
	return out, nil
}

// IsCredentialNotFound reports whether err is the not-found
// sentinel. Mirrors the channel-state IsNotFound helper so callers
// don't need to import pkg/types.
func IsCredentialNotFound(err error) bool {
	return errors.Is(err, types.ErrNotFound)
}

func removeString(haystack []string, needle string) []string {
	out := haystack[:0]
	for _, h := range haystack {
		if h != needle {
			out = append(out, h)
		}
	}
	return out
}
