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

// channelStateApplyTimeout bounds raft.Apply for channel state
// writes. Channel state is small + frequent — keep the timeout
// modest so a write hiccup doesn't block the gateway poll loop
// for long.
const channelStateApplyTimeout = 2 * time.Second

// channelStateKey composes the bbolt key from channel + key. The
// "<channel>:<key>" shape is consistent across reads + writes; ":"
// is forbidden in either component to keep the encoding unambiguous.
func channelStateKey(channel, key string) (string, error) {
	if channel == "" {
		return "", errors.New("channel state: channel required")
	}
	if key == "" {
		return "", errors.New("channel state: key required")
	}
	for _, s := range []string{channel, key} {
		for _, r := range s {
			if r == ':' {
				return "", fmt.Errorf("channel state: %q must not contain ':'", s)
			}
		}
	}
	return channel + ":" + key, nil
}

// ChannelStateService exposes raft-backed Get/Put for arbitrary
// channel resume state. Lives in the memory package because it
// shares the FSM + bbolt store with all other consensus state.
//
// Reads are local (no raft round-trip). Writes go through raft so
// state replicates across the cluster — any node that becomes
// leader can resume polling with the latest known offset.
type ChannelStateService struct {
	raft  *RaftNode
	store *Store
}

// NewChannelStateService wires the service against an existing
// Raft + Store. Nil raft → writes return an error; reads still
// work locally if store is non-nil. That asymmetry matches the
// pattern used elsewhere in this package (memory.Service etc).
func NewChannelStateService(raft *RaftNode, store *Store) *ChannelStateService {
	return &ChannelStateService{raft: raft, store: store}
}

// Get returns the value for channel+key, or types.ErrNotFound when
// nothing's been written. Reads bypass raft — they hit the local
// FSM directly.
func (s *ChannelStateService) Get(_ context.Context, channel, key string) ([]byte, error) {
	bktKey, err := channelStateKey(channel, key)
	if err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, errors.New("channel state: store not wired")
	}
	raw, err := s.store.Get(BucketChannelState, bktKey)
	if err != nil {
		return nil, err
	}
	var rec lobslawv1.ChannelStateRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("channel state: unmarshal %s/%s: %w", channel, key, err)
	}
	return rec.Value, nil
}

// Put writes value via raft so it replicates. Leader-only — followers
// get an error so the caller (typically a singleton-gated channel
// loop) can decide whether to retry, defer, or surface.
func (s *ChannelStateService) Put(_ context.Context, channel, key string, value []byte) error {
	bktKey, err := channelStateKey(channel, key)
	if err != nil {
		return err
	}
	if s.raft == nil {
		return errors.New("channel state: raft not wired")
	}
	if !s.raft.IsLeader() {
		return fmt.Errorf("channel state: not the raft leader; current leader is %s", s.raft.LeaderAddress())
	}
	rec := &lobslawv1.ChannelStateRecord{
		Channel:   channel,
		Key:       key,
		Value:     value,
		UpdatedAt: timestamppb.Now(),
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      bktKey,
		Payload: &lobslawv1.LogEntry_ChannelState{ChannelState: rec},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("channel state: marshal: %w", err)
	}
	if _, err := s.raft.Apply(data, channelStateApplyTimeout); err != nil {
		return fmt.Errorf("channel state: raft apply: %w", err)
	}
	return nil
}

// IsNotFound reports whether err is the "no record" sentinel.
// Mirrors types.ErrNotFound so callers don't have to import the
// types package just to check.
func IsNotFound(err error) bool {
	return errors.Is(err, types.ErrNotFound)
}
