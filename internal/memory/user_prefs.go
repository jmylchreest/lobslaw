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
)

// userPrefsApplyTimeout caps raft.Apply for prefs writes. Writes
// are infrequent (configuration-style, not per-turn) so 5s is
// generous.
const userPrefsApplyTimeout = 5 * time.Second

// UserPrefsService manages the user_prefs bucket. Reads are local;
// writes go through Raft so prefs replicate cluster-wide.
//
// Plaintext storage: channel addresses (telegram chat_id, etc.) and
// timezone strings aren't sensitive. The encryption tax we pay for
// the credentials bucket isn't justified here, and plaintext keeps
// the bucket inspectable via standard raft introspection.
type UserPrefsService struct {
	raft  *RaftNode
	store *Store
}

// NewUserPrefsService wires the service. Callers handing nil raft
// can still Get/List from the local replica; Put/Delete will error.
func NewUserPrefsService(raft *RaftNode, store *Store) *UserPrefsService {
	return &UserPrefsService{raft: raft, store: store}
}

// Get returns the prefs record for user_id, or types.ErrNotFound
// when none exists. Reads are local — no raft round-trip.
func (s *UserPrefsService) Get(_ context.Context, userID string) (*lobslawv1.UserPreferences, error) {
	if s.store == nil {
		return nil, errors.New("user_prefs: store not wired")
	}
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	raw, err := s.store.Get(BucketUserPrefs, userID)
	if err != nil {
		return nil, err
	}
	var rec lobslawv1.UserPreferences
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("user_prefs: unmarshal %s: %w", userID, err)
	}
	return &rec, nil
}

// List returns every prefs record. Used by the notification service
// to broadcast across the cluster's known users; also by operator
// CLIs to inspect bindings.
func (s *UserPrefsService) List(_ context.Context) ([]*lobslawv1.UserPreferences, error) {
	if s.store == nil {
		return nil, errors.New("user_prefs: store not wired")
	}
	var out []*lobslawv1.UserPreferences
	err := s.store.ForEach(BucketUserPrefs, func(_ string, raw []byte) error {
		var rec lobslawv1.UserPreferences
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return err
		}
		out = append(out, &rec)
		return nil
	})
	return out, err
}

// Put writes a prefs record. Leader-only. The user_id is the
// bucket key — a Put against an existing user_id overwrites that
// record (last-writer-wins, no merge semantics). Callers wanting
// merge behaviour read first, modify, write back.
//
// CreatedAt is stamped on first write only; UpdatedAt is stamped
// on every write so operators can see when a binding last changed.
func (s *UserPrefsService) Put(ctx context.Context, p *lobslawv1.UserPreferences) error {
	if p == nil {
		return errors.New("user_prefs: nil record")
	}
	if s.raft == nil {
		return errors.New("user_prefs: raft not wired")
	}
	if !s.raft.IsLeader() {
		return fmt.Errorf("user_prefs: not the raft leader; current leader is %s", s.raft.LeaderAddress())
	}
	if err := validateUserID(p.UserId); err != nil {
		return err
	}
	if err := validatePrefs(p); err != nil {
		return err
	}
	now := timestamppb.New(time.Now())
	if existing, err := s.Get(ctx, p.UserId); err == nil && existing.CreatedAt != nil {
		p.CreatedAt = existing.CreatedAt
	} else if p.CreatedAt == nil {
		p.CreatedAt = now
	}
	p.UpdatedAt = now

	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      p.UserId,
		Payload: &lobslawv1.LogEntry_UserPrefs{UserPrefs: p},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("user_prefs: marshal: %w", err)
	}
	if _, err := s.raft.Apply(data, userPrefsApplyTimeout); err != nil {
		return fmt.Errorf("user_prefs: raft apply: %w", err)
	}
	return nil
}

// Delete removes a prefs record. Leader-only.
func (s *UserPrefsService) Delete(_ context.Context, userID string) error {
	if s.raft == nil {
		return errors.New("user_prefs: raft not wired")
	}
	if !s.raft.IsLeader() {
		return fmt.Errorf("user_prefs: not the raft leader; current leader is %s", s.raft.LeaderAddress())
	}
	if err := validateUserID(userID); err != nil {
		return err
	}
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_DELETE,
		Id: userID,
		Payload: &lobslawv1.LogEntry_UserPrefs{
			UserPrefs: &lobslawv1.UserPreferences{UserId: userID},
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("user_prefs: marshal: %w", err)
	}
	if _, err := s.raft.Apply(data, userPrefsApplyTimeout); err != nil {
		return fmt.Errorf("user_prefs: raft apply: %w", err)
	}
	return nil
}

// FindByChannelAddress returns the user_id whose preferences list
// includes the given (channel, address) pair, or ErrNotFound when
// no user is bound to that channel address. Used by the inbound
// gateway to resolve channel-specific IDs (telegram chat_id) to
// canonical user_ids.
//
// Linear scan over the prefs bucket: cheap because the bucket is
// small (one entry per user, expected ≤ small handfuls). When that
// stops being true we add an inverted-index bucket.
func (s *UserPrefsService) FindByChannelAddress(ctx context.Context, channel, address string) (*lobslawv1.UserPreferences, error) {
	if channel == "" || address == "" {
		return nil, errors.New("user_prefs: channel + address required")
	}
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		for _, c := range p.Channels {
			if c.Type == channel && c.Address == address {
				return p, nil
			}
		}
	}
	return nil, fmt.Errorf("user_prefs: no user bound to %s:%s", channel, address)
}

// validateUserID guards bucket-key invariants. Empty fails;
// embedded ":" fails so the key can later become a composite
// without ambiguity.
func validateUserID(userID string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return errors.New("user_prefs: user_id required")
	}
	if strings.ContainsAny(userID, ":/") {
		return fmt.Errorf("user_prefs: user_id %q must not contain ':' or '/'", userID)
	}
	return nil
}

// validatePrefs guards record-shape invariants. Timezone, when
// non-empty, must parse as an IANA zone (operator typo here is
// painful to debug at render-time).
func validatePrefs(p *lobslawv1.UserPreferences) error {
	if tz := strings.TrimSpace(p.Timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("user_prefs: timezone %q is not a valid IANA zone: %w", tz, err)
		}
	}
	for i, c := range p.Channels {
		if strings.TrimSpace(c.Type) == "" {
			return fmt.Errorf("user_prefs: channels[%d].type is required", i)
		}
		if strings.TrimSpace(c.Address) == "" {
			return fmt.Errorf("user_prefs: channels[%d].address is required", i)
		}
	}
	return nil
}
