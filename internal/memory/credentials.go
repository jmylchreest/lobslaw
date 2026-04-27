package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
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
// Leader-only — followers return an error. Used by the OAuth flow
// (initial token write + refresh rotation) and by Grant/Revoke
// (ACL mutation only — tokens unchanged).
func (s *CredentialService) Put(_ context.Context, p *PlaintextCredential) error {
	if p == nil {
		return errors.New("credentials: nil credential")
	}
	if s.raft == nil {
		return errors.New("credentials: raft not wired")
	}
	if !s.raft.IsLeader() {
		return fmt.Errorf("credentials: not the raft leader; current leader is %s", s.raft.LeaderAddress())
	}
	key, err := CredentialKey(p.Provider, p.Subject)
	if err != nil {
		return err
	}
	rec, err := s.encrypt(p)
	if err != nil {
		return err
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      key,
		Payload: &lobslawv1.LogEntry_Credential{Credential: rec},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("credentials: marshal: %w", err)
	}
	if _, err := s.raft.Apply(data, credentialApplyTimeout); err != nil {
		return fmt.Errorf("credentials: raft apply: %w", err)
	}
	return nil
}

// Delete removes a credential by (provider, subject). Leader-only.
// Used by the "credentials revoke" CLI/builtin.
func (s *CredentialService) Delete(_ context.Context, provider, subject string) error {
	if s.raft == nil {
		return errors.New("credentials: raft not wired")
	}
	if !s.raft.IsLeader() {
		return fmt.Errorf("credentials: not the raft leader; current leader is %s", s.raft.LeaderAddress())
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
	if _, err := s.raft.Apply(data, credentialApplyTimeout); err != nil {
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
	cred, err := s.Get(ctx, provider, subject)
	if err != nil {
		return err
	}
	if !contains(cred.AllowedSkills, skill) {
		cred.AllowedSkills = append(cred.AllowedSkills, skill)
	}
	if cred.AllowedScopesPerSkill == nil {
		cred.AllowedScopesPerSkill = make(map[string][]string)
	}
	// Verify the requested scopes are a subset of what was granted
	// at OAuth time. A skill can't be granted scopes the credential
	// doesn't have.
	for _, sc := range scopes {
		if !contains(cred.Scopes, sc) {
			return fmt.Errorf("credentials: cannot grant scope %q — not in credential's granted scopes %v", sc, cred.Scopes)
		}
	}
	cred.AllowedScopesPerSkill[skill] = append([]string(nil), scopes...)
	return s.Put(ctx, cred)
}

// Revoke removes a skill from the credential's ACL. The credential
// itself stays — Revoke only narrows access. Use Delete to remove
// the credential entirely.
func (s *CredentialService) Revoke(ctx context.Context, provider, subject, skill string) error {
	cred, err := s.Get(ctx, provider, subject)
	if err != nil {
		return err
	}
	cred.AllowedSkills = removeString(cred.AllowedSkills, skill)
	delete(cred.AllowedScopesPerSkill, skill)
	return s.Put(ctx, cred)
}

// ScopesAllowedForSkill returns the scope subset a given skill may
// request from this credential. Empty slice when the skill isn't
// in AllowedSkills. Used by the credentials_request builtin to
// validate per-skill scope subsetting.
func (s *CredentialService) ScopesAllowedForSkill(p *PlaintextCredential, skill string) []string {
	if p == nil || !contains(p.AllowedSkills, skill) {
		return nil
	}
	return p.AllowedScopesPerSkill[skill]
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
		RefreshToken: string(refresh),
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

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
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
