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
// Writes are revision-checked by the FSM, including forwarded writes.
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
// updated_at is stamped before proposal and replicated with the state.
func (s *SoulTuneService) Put(ctx context.Context, state *lobslawv1.SoulTuneState, expectedRevision uint64) (*lobslawv1.SoulTuneRecord, error) {
	if state == nil {
		return nil, errors.New("soul tune: state required")
	}
	if s.raft == nil {
		return nil, errors.New("soul tune: raft not wired")
	}
	prev, err := s.Get(ctx)
	if err != nil {
		return nil, err
	}
	if prev.GetRevision() != expectedRevision {
		return nil, fmt.Errorf("%w: soul changed; read current state and retry", ErrClaimConflict)
	}
	return s.put(ctx, state, prev)
}

func (s *SoulTuneService) put(ctx context.Context, state *lobslawv1.SoulTuneState, prev *lobslawv1.SoulTuneRecord) (*lobslawv1.SoulTuneRecord, error) {
	state = proto.Clone(state).(*lobslawv1.SoulTuneState)
	state.UpdatedAt = timestamppb.Now()
	hist := append([]*lobslawv1.SoulTuneState(nil), prev.GetHistory()...)
	prior := prev.GetCurrent()
	// The empty overlay is a real prior version: undoing the first edit
	// restores inheritance, even if the baseline has changed in the meantime.
	if prior == nil {
		prior = &lobslawv1.SoulTuneState{}
	}
	hist = append(hist, prior)
	if len(hist) > MaxSoulTuneHistory {
		hist = hist[len(hist)-MaxSoulTuneHistory:]
	}
	revision := prev.GetRevision()
	rec := &lobslawv1.SoulTuneRecord{Current: state, History: hist, Revision: revision + 1}
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: SoulTuneRecordID,
		ExpectedRevision: &revision,
		Payload:          &lobslawv1.LogEntry_SoulTune{SoulTune: rec},
	})
	if err != nil {
		return nil, err
	}
	res, err := s.raft.ApplyOrForward(ctx, data, soulTuneApplyTimeout)
	if err != nil {
		return nil, fmt.Errorf("soul tune: raft apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return nil, applyErr
	}
	return rec, nil
}

// Rollback appends a restoration as a new revision. The CAS covers the
// history selection too, so a concurrent edit cannot silently be discarded.
func (s *SoulTuneService) Rollback(ctx context.Context, steps int) (*lobslawv1.SoulTuneRecord, error) {
	if steps < 1 {
		return nil, errors.New("soul tune: steps must be >= 1")
	}
	if s.raft == nil {
		return nil, errors.New("soul tune: raft not wired")
	}
	prev, err := s.Get(ctx)
	if err != nil {
		return nil, err
	}
	if steps > len(prev.GetHistory()) {
		return nil, fmt.Errorf("soul tune: only %d history entries; cannot rollback %d steps", len(prev.GetHistory()), steps)
	}
	picked := prev.History[len(prev.History)-steps]
	return s.put(ctx, picked, prev)
}
