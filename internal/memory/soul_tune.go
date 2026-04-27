package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// soulTuneApplyTimeout caps raft.Apply for soul writes. Personality
// edits are user-pace (seconds between turns) so 5s is generous.
const soulTuneApplyTimeout = 5 * time.Second

// MaxSoulTuneHistory is the number of past tunes retained for
// HistoryRollback. Same magic-20 the file-based history used; chosen
// because operators rarely need to walk back further and the record
// stays under a few KB even with all 20 versions.
const MaxSoulTuneHistory = 20

// SoulTuneService exposes raft-backed Get/Put for the cluster-wide
// soul tune record. Reads are local (FSM); writes go through raft so
// every node converges.
//
// Leader-only writes — followers return an error so the calling code
// (typically the agent on the leader) can surface "not the leader"
// to the user instead of silently dropping.
type SoulTuneService struct {
	raft  *RaftNode
	store *Store
}

// NewSoulTuneService wires the service against an existing Raft +
// Store. Nil raft → writes return an error; reads still work locally
// when store is non-nil. Matches ChannelStateService asymmetry.
func NewSoulTuneService(raft *RaftNode, store *Store) *SoulTuneService {
	return &SoulTuneService{raft: raft, store: store}
}

// Get returns the current SoulTuneRecord. Returns (nil, nil) when
// nothing has been written yet — the Adjuster treats this as "no
// overlay; serve baseline". Errors are reserved for unmarshal /
// store failures.
func (s *SoulTuneService) Get(_ context.Context) (*lobslawv1.SoulTuneRecord, error) {
	if s.store == nil {
		return nil, errors.New("soul tune: store not wired")
	}
	raw, err := s.store.Get(BucketSoulTune, SoulTuneRecordID)
	if err != nil {
		if errors.Is(err, types.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var rec lobslawv1.SoulTuneRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("soul tune: unmarshal: %w", err)
	}
	return &rec, nil
}

// Put writes a new tune state. The service handles history rotation:
// the previous current is appended to history (capped at
// MaxSoulTuneHistory), and the supplied state becomes the new current.
// updated_at is stamped on the leader so all followers see the same
// timestamp via raft replication.
func (s *SoulTuneService) Put(ctx context.Context, state *lobslawv1.SoulTuneState) error {
	if state == nil {
		return errors.New("soul tune: state required")
	}
	if s.raft == nil {
		return errors.New("soul tune: raft not wired")
	}
	if !s.raft.IsLeader() {
		return fmt.Errorf("soul tune: not the raft leader; current leader is %s", s.raft.LeaderAddress())
	}
	prev, err := s.Get(ctx)
	if err != nil {
		return fmt.Errorf("soul tune: read current: %w", err)
	}
	state.UpdatedAt = timestamppb.Now()
	rec := &lobslawv1.SoulTuneRecord{Current: state}
	if prev != nil {
		hist := append([]*lobslawv1.SoulTuneState(nil), prev.History...)
		if prev.Current != nil {
			hist = append(hist, prev.Current)
		}
		if len(hist) > MaxSoulTuneHistory {
			hist = hist[len(hist)-MaxSoulTuneHistory:]
		}
		rec.History = hist
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      SoulTuneRecordID,
		Payload: &lobslawv1.LogEntry_SoulTune{SoulTune: rec},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("soul tune: marshal: %w", err)
	}
	if _, err := s.raft.Apply(data, soulTuneApplyTimeout); err != nil {
		return fmt.Errorf("soul tune: raft apply: %w", err)
	}
	return nil
}

// Rollback restores a previous state from history. steps=1 reverts
// the most recent change, steps=2 the one before, etc. Returns the
// state that was promoted to current (or an error if no history is
// available at that depth).
func (s *SoulTuneService) Rollback(ctx context.Context, steps int) (*lobslawv1.SoulTuneState, error) {
	if steps < 1 {
		return nil, errors.New("soul tune: steps must be >= 1")
	}
	prev, err := s.Get(ctx)
	if err != nil {
		return nil, err
	}
	if prev == nil || len(prev.History) == 0 {
		return nil, fmt.Errorf("soul tune: no history available")
	}
	if steps > len(prev.History) {
		return nil, fmt.Errorf("soul tune: only %d history entries; cannot rollback %d steps", len(prev.History), steps)
	}
	idx := len(prev.History) - steps
	picked := prev.History[idx]
	if picked == nil {
		return nil, fmt.Errorf("soul tune: history entry at depth %d is nil", steps)
	}
	if err := s.Put(ctx, picked); err != nil {
		return nil, err
	}
	return picked, nil
}
